package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// Loader is the read side of the state store. Taking the interface rather than
// the store keeps this package testable without GitHub.
type Loader interface {
	Load(ctx context.Context) (state.State, state.Revision, error)
}

// Logger matches the logger the rest of crq passes around.
type Logger interface {
	Printf(format string, args ...any)
}

// Options is everything the server needs that it must not decide for itself.
type Options struct {
	Addr        string
	MinInterval time.Duration
	Inflight    time.Duration
	// Bots is the fleet's reviewer list, used only when Resolve is nil.
	Bots []BotName
	// Resolve answers which reviewers run on one repository. It takes the whole
	// state, not just that repository's override, because the answer also
	// depends on the fleet's recorded defaults — resolving from the override
	// alone showed this host's env list however the fleet had been configured.
	// An empty repo asks for the fleet's own answer.
	Resolve func(st state.State, repo string) []BotName
	// WeeklyLimit is the vendor's weekly fair-use threshold. 0 counts without
	// forecasting.
	WeeklyLimit int
	// Poll is how often the state ref is re-read. There is no webhook for a git
	// ref, so this is a poll by necessity; a push only happens when Rev moves.
	Poll time.Duration
	// Assets is the built SPA. Nil serves a plain page explaining how to build
	// it, so `crq serve` is still useful from a source checkout.
	Assets fs.FS
	Log    Logger
	Now    func() time.Time
	// Fleet is the configuration the settings and setup pages display.
	Fleet FleetConfig
	// Host names the machine this server runs on, so the tool list can say
	// whose PATH it describes.
	Host string
	// Token authenticates icon fetches. Repository favicons live in private
	// repos too, and the browser must never hold this.
	Token string
	// EnrollFor resolves whether crq reviews one repository. Nil falls back to
	// reading the env lists alone, which is what the dashboard did before
	// enrollment records existed.
	EnrollFor EnrollFor
	// Discoverer lists the repositories in scope for the repo picker. Nil means
	// the picker reports that discovery is not configured rather than showing an
	// empty list, which would read as "you have no repositories".
	Discoverer Discoverer
	// Coster estimates what a round would cost. Optional: without it the PR page
	// simply shows no price, rather than a wrong one.
	Coster Coster
	// SolverFor resolves a repository's fix-session settings. Nil leaves the
	// repository page without a solver card rather than showing env values that
	// no repository record could change.
	SolverFor SolverFor
	// Observer supplies the per-PR findings. Optional: without it the PR page
	// still renders its state layer.
	Observer Observer
	// Actor performs writes. Nil, or ReadOnly, makes every action endpoint
	// refuse — useful when pointing a dashboard at someone else's fleet.
	Actor    Actor
	ReadOnly bool
}

// Server holds the latest snapshot and fans it out to connected browsers.
type Server struct {
	opts   Options
	loader Loader

	mu      sync.RWMutex
	last    Snapshot
	lastRev int64
	loadErr error
	subs    map[chan []byte]struct{}
	// tools are probed once: LookPath per refresh would be wasted work, and the
	// answer only changes when someone installs something.
	tools []Tool
	icons *Icons
	// lastState is kept so a per-PR request can render without another read of
	// the state ref; the poller already has it.
	lastState    state.State
	observer     Observer
	observations *observeCache
	actor        Actor
	events       *eventLog
	discovered   *discoverCache
	costs        *costCache
}

func New(loader Loader, opts Options) *Server {
	if opts.Poll <= 0 {
		opts.Poll = 5 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Server{opts: opts, loader: loader, subs: map[chan []byte]struct{}{},
		tools: LocalTools(), icons: NewIcons(opts.Token), observer: opts.Observer,
		observations: &observeCache{}, actor: opts.Actor,
		events: newEventLog(300), discovered: &discoverCache{}, costs: &costCache{}}
}

// Run polls the state ref and serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go s.watch(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/icon/{kind}/{name...}", s.handleIcon)
	mux.HandleFunc("GET /api/pr/{owner}/{name}/{pr}", s.handlePR)
	mux.HandleFunc("GET /api/discover", s.handleDiscover)
	mux.HandleFunc("POST /api/action/{action}", s.handleAction)
	mux.Handle("/", s.assets())

	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if s.opts.Log != nil {
		s.opts.Log.Printf("dashboard on http://%s", s.opts.Addr)
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// watch re-reads the state ref and pushes a snapshot whenever Rev moves. A
// failed read never clears the last good snapshot — stale-but-labelled beats
// blank, and the error rides along so the UI can say so.
func (s *Server) watch(ctx context.Context) {
	tick := time.NewTicker(s.opts.Poll)
	defer tick.Stop()
	s.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.refresh(ctx)
		}
	}
}

func (s *Server) refresh(ctx context.Context) {
	st, _, err := s.loader.Load(ctx)
	if err != nil {
		s.mu.Lock()
		s.loadErr = err
		s.mu.Unlock()
		return
	}
	now := s.opts.Now()

	// Derive events before the snapshot so the feed it carries is current.
	s.mu.RLock()
	prev := s.lastState
	s.mu.RUnlock()
	s.events.add(diffStates(prev, st, now)...)

	botsFor := s.botsFor(&st)
	ov := BuildOverview(st, now, s.opts.MinInterval, s.opts.Inflight, botsFor, s.opts.WeeklyLimit)
	// Read alongside the snapshot rather than cached: it is a state read the
	// server has already paid for, and a settings page showing a stale default
	// is how two people overwrite each other.
	var fleet *FleetSettings
	var env []EnvSetting
	if s.actor != nil {
		if f, err := s.actor.Fleet(ctx); err == nil {
			fleet = f
		}
		env = s.actor.EnvSettings(st)
	}
	snap := BuildFleet(st, s.opts.Fleet, ov, s.tools, s.opts.Host, now, botsFor, s.opts.EnrollFor, fleet, s.opts.SolverFor, env)
	snap.Events = s.events.list()

	s.mu.Lock()
	changed := ov.Rev != s.lastRev
	s.last, s.lastRev, s.loadErr, s.lastState = snap, ov.Rev, nil, st
	subs := make([]chan []byte, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	if !changed && len(subs) > 0 {
		// Countdowns tick client-side, so an unchanged rev needs no push. Only
		// the clock moved.
		return
	}
	payload, err := json.Marshal(snap)
	if err != nil {
		return
	}
	for _, ch := range subs {
		select {
		case ch <- payload:
		default: // a browser that cannot keep up gets the next one
		}
	}
}

func (s *Server) snapshot() (Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last, s.loadErr
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot()
	if snap.Overview.Rev == 0 && err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot()
	if snap.Overview.Rev == 0 && err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snap.Overview)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot()
	body := map[string]any{"rev": snap.Overview.Rev, "ok": err == nil}
	if err != nil {
		body["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

// handleEvents streams whole snapshots. The state blob is small, so replacing
// it wholesale is simpler than diffing and makes reconnection trivially correct.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 4)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	// Send the current state immediately so a fresh tab paints without waiting
	// for the next change.
	if snap, err := s.snapshot(); err == nil || snap.Overview.Rev != 0 {
		if payload, err := json.Marshal(snap); err == nil {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case payload := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// assets serves the embedded SPA, falling back to index.html so client-side
// routes survive a reload.
func (s *Server) assets() http.Handler {
	if s.opts.Assets == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, unbuiltPage)
		})
	}
	files := http.FileServer(http.FS(s.opts.Assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if _, err := fs.Stat(s.opts.Assets, path); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
				path = ""
			}
		}
		// The bundles are content-hashed, so a name that resolves at all
		// resolves to the same bytes for ever and can be cached hard. The page
		// that NAMES them cannot: a cached index.html keeps asking for the
		// bundle it was built against, so a restarted server serves new assets
		// nobody ever requests and the dashboard silently stays a version behind.
		if strings.HasPrefix(path, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

const unbuiltPage = `<!doctype html><meta charset="utf-8">
<title>Code Review Queue</title>
<style>body{font:14px/1.6 system-ui,sans-serif;margin:60px auto;max-width:640px;color:#1B2430}
code{background:#EEF0F3;border-radius:5px;padding:1px 6px;font-size:13px}</style>
<h1>Dashboard assets are not built</h1>
<p>The API is running — <a href="/api/overview">/api/overview</a> works — but the web app
was not compiled into this binary.</p>
<p>Build it from the repository root:</p>
<p><code>cd web &amp;&amp; bun install &amp;&amp; bun run build</code></p>
<p>then rebuild crq. The assets are embedded from <code>web/dist</code>.</p>
`

// botsFor binds the reviewer resolver to one loaded state, so a row can ask
// about its own repository without another state read.
func (s *Server) botsFor(st *state.State) BotsFor {
	return func(repo string) []BotName {
		if s.opts.Resolve == nil {
			return s.opts.Bots
		}
		return s.opts.Resolve(*st, repo)
	}
}
