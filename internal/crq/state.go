package crq

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	statePath     = "state.json"
	dashboardPath = "dashboard.md"
	stateBegin    = "<!-- crq:state"
	stateEnd      = "-->"
)

type State struct {
	Version          int                     `json:"v"`
	Rev              int64                   `json:"rev"`
	NextSeq          int64                   `json:"next_seq"`
	Queue            []QueueItem             `json:"queue"`
	InFlight         *InFlight               `json:"in_flight"`
	LastFired        *time.Time              `json:"last_fired"`
	Warn             string                  `json:"warn,omitempty"`
	Fired            map[string]string       `json:"fired"`
	AwaitingFeedback map[string]FeedbackWait `json:"awaiting_feedback,omitempty"`
	History          []HistoryItem           `json:"history"`
	Blocked          Blocked                 `json:"blocked"`
	Leader           *LeaderLease            `json:"leader,omitempty"`
	GCAt             *time.Time              `json:"gc_at,omitempty"`
	UpdatedAt        *time.Time              `json:"wrote_at,omitempty"`
	DashboardSHA     string                  `json:"dashboard_sha,omitempty"`
}

type QueueItem struct {
	Seq        int64     `json:"seq"`
	Owner      string    `json:"owner"`
	Repo       string    `json:"repo"`
	PR         int       `json:"pr"`
	Host       string    `json:"host"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}

type InFlight struct {
	Seq            int64      `json:"seq"`
	Repo           string     `json:"repo"`
	PR             int        `json:"pr"`
	Head           string     `json:"head,omitempty"`
	Token          string     `json:"token"`
	Phase          string     `json:"phase"`
	ReservedAt     time.Time  `json:"reserved_at"`
	FiredAt        *time.Time `json:"fired_at,omitempty"`
	FiredCommentID int64      `json:"fired_comment_id,omitempty"`
	ByHost         string     `json:"by_host"`
}

type FeedbackWait struct {
	Repo      string    `json:"repo"`
	PR        int       `json:"pr"`
	Head      string    `json:"head"`
	StartedAt time.Time `json:"started_at"`
	Deadline  time.Time `json:"deadline"`
	ByHost    string    `json:"by_host,omitempty"`
}

type HistoryItem struct {
	Repo   string    `json:"repo"`
	PR     int       `json:"pr"`
	Commit string    `json:"commit,omitempty"`
	At     time.Time `json:"at"`
	Host   string    `json:"host"`
}

type Blocked struct {
	Scope        string     `json:"scope"`
	BlockedUntil *time.Time `json:"blocked_until"`
	Remaining    *int       `json:"remaining"`
	Source       string     `json:"source"`
	CheckedAt    *time.Time `json:"checked_at"`
	CalibAskedAt *time.Time `json:"calib_asked_at"`
}

type LeaderLease struct {
	Owner     string    `json:"owner"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func DefaultState(cfg Config) State {
	return State{
		Version: 1,
		Rev:     0,
		Fired:   map[string]string{},
		Blocked: Blocked{Scope: strings.Join(cfg.Scope, ","), Source: "init"},
	}
}

func (s *State) Normalize(cfg Config) {
	if s.Version == 0 {
		s.Version = 1
	}
	if s.Fired == nil {
		s.Fired = map[string]string{}
	}
	if s.AwaitingFeedback == nil {
		s.AwaitingFeedback = map[string]FeedbackWait{}
	}
	for i := range s.Queue {
		s.Queue[i].Repo = NormalizeRepo(s.Queue[i].Repo)
		s.Queue[i].Owner = ownerOf(s.Queue[i].Repo)
	}
	if s.InFlight != nil {
		s.InFlight.Repo = NormalizeRepo(s.InFlight.Repo)
	}
	for i := range s.History {
		s.History[i].Repo = NormalizeRepo(s.History[i].Repo)
	}
	if s.Blocked.Scope == "" {
		s.Blocked.Scope = strings.Join(cfg.Scope, ",")
	}
	if s.Blocked.Source == "" {
		s.Blocked.Source = "init"
	}
	folded := map[string]string{}
	for k, v := range s.Fired {
		folded[strings.ToLower(k)] = v
	}
	s.Fired = folded
	awaiting := map[string]FeedbackWait{}
	for k, wait := range s.AwaitingFeedback {
		repo, pr := wait.Repo, wait.PR
		if repo == "" || pr <= 0 {
			repo, pr = splitQueueKey(k)
		}
		repo = NormalizeRepo(repo)
		if repo == "" || pr <= 0 || wait.Head == "" {
			continue
		}
		wait.Repo = repo
		wait.PR = pr
		key := QueueKey(repo, pr)
		awaiting[key] = wait
		if s.Fired[key] == "" {
			s.Fired[key] = wait.Head
		}
	}
	s.AwaitingFeedback = awaiting
	if cfg.FiredMax > 0 && len(s.Fired) > cfg.FiredMax {
		// Protect markers for recently-fired heads (those still in History) from
		// eviction. Normalize runs right after a fire records st.Fired[key]=head, and
		// a plain lexicographic trim could drop that just-written marker for a repo
		// whose name sorts early — making crq forget the head was already requested
		// and fire a duplicate review. Only the older remainder is trimmed.
		type kv struct {
			Key string
			Val string
		}
		type historyMarker struct {
			Key    string
			Commit string
			At     time.Time
			Index  int
		}
		recent := make([]historyMarker, 0, len(s.History))
		for i, h := range s.History {
			key := strings.ToLower(QueueKey(h.Repo, h.PR))
			if fired := s.Fired[key]; fired != "" && (h.Commit == "" || h.Commit == fired) {
				recent = append(recent, historyMarker{Key: key, Commit: fired, At: h.At, Index: i})
			}
		}
		sort.SliceStable(recent, func(i, j int) bool {
			if !recent[i].At.Equal(recent[j].At) {
				return recent[i].At.After(recent[j].At)
			}
			return recent[i].Index < recent[j].Index
		})
		protected := map[string]string{}
		awaitingMarkers := make([]historyMarker, 0, len(s.AwaitingFeedback))
		for _, wait := range s.AwaitingFeedback {
			key := QueueKey(wait.Repo, wait.PR)
			if fired := s.Fired[key]; fired != "" && fired == wait.Head {
				awaitingMarkers = append(awaitingMarkers, historyMarker{Key: key, Commit: fired, At: wait.StartedAt})
			}
		}
		sort.SliceStable(awaitingMarkers, func(i, j int) bool {
			return awaitingMarkers[i].At.After(awaitingMarkers[j].At)
		})
		for _, marker := range awaitingMarkers {
			if len(protected) >= cfg.FiredMax {
				break
			}
			protected[marker.Key] = marker.Commit
		}
		for _, marker := range recent {
			if len(protected) >= cfg.FiredMax {
				break
			}
			if _, ok := protected[marker.Key]; ok {
				continue
			}
			protected[marker.Key] = marker.Commit
		}
		items := make([]kv, 0, len(s.Fired))
		for k, v := range s.Fired {
			if _, ok := protected[k]; ok {
				continue
			}
			items = append(items, kv{k, v})
		}
		budget := cfg.FiredMax - len(protected)
		if budget < 0 {
			budget = 0
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })
		if len(items) > budget {
			items = items[len(items)-budget:]
		}
		s.Fired = protected
		for _, item := range items {
			s.Fired[item.Key] = item.Val
		}
	}
}

func splitQueueKey(key string) (string, int) {
	repo, prText, ok := strings.Cut(strings.ToLower(key), "#")
	if !ok {
		return "", 0
	}
	pr, err := strconv.Atoi(prText)
	if err != nil {
		return "", 0
	}
	return NormalizeRepo(repo), pr
}

func NormalizeRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	repo = strings.TrimSuffix(repo, ".git")
	return strings.ToLower(repo)
}

func QueueKey(repo string, pr int) string {
	return fmt.Sprintf("%s#%d", NormalizeRepo(repo), pr)
}

func (s State) Contains(repo string, pr int) bool {
	repo = NormalizeRepo(repo)
	if s.InFlight != nil && s.InFlight.Repo == repo && s.InFlight.PR == pr {
		return true
	}
	for _, item := range s.Queue {
		if item.Repo == repo && item.PR == pr {
			return true
		}
	}
	return false
}

func (s State) SortedQueue() []QueueItem {
	out := append([]QueueItem(nil), s.Queue...)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (s State) SortedAwaitingFeedback() []FeedbackWait {
	out := make([]FeedbackWait, 0, len(s.AwaitingFeedback))
	for _, wait := range s.AwaitingFeedback {
		out = append(out, wait)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Deadline.Equal(out[j].Deadline) {
			return out[i].Deadline.Before(out[j].Deadline)
		}
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].PR < out[j].PR
	})
	return out
}

const crqProjectURL = "https://github.com/kristofferR/coderabbit-queue"

func dashboardLoc(cfg Config) *time.Location {
	if cfg.Timezone != "" {
		if loc, err := time.LoadLocation(cfg.Timezone); err == nil {
			return loc
		}
	}
	return time.UTC
}

func fmtStamp(t *time.Time, loc *time.Location) string {
	if t == nil {
		return "—"
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

func minutesUntil(t time.Time, now time.Time) int {
	mins := int(t.Sub(now).Minutes()) + 1
	if mins < 1 {
		mins = 1
	}
	return mins
}

func renderDashboard(state State, cfg Config) string {
	loc := dashboardLoc(cfg)
	now := time.Now().UTC()
	queue := state.SortedQueue()
	awaiting := state.SortedAwaitingFeedback()
	blocked := state.Blocked.BlockedUntil != nil && state.Blocked.BlockedUntil.After(now)

	var b strings.Builder
	fmt.Fprintf(&b, "# 🐰 crq — CodeRabbit review queue\n\n")

	switch {
	case blocked:
		fmt.Fprintf(&b, "### 🔴 Blocked — next review in ~%dm\n\n", minutesUntil(*state.Blocked.BlockedUntil, now))
	case state.InFlight != nil:
		fmt.Fprintf(&b, "### 🟡 Reviewing %s#%d\n\n", state.InFlight.Repo, state.InFlight.PR)
	case len(awaiting) > 0:
		fmt.Fprintf(&b, "### 🟡 Awaiting feedback for %s#%d\n\n", awaiting[0].Repo, awaiting[0].PR)
	case len(queue) > 0:
		fmt.Fprintf(&b, "### 🟠 %d queued\n\n", len(queue))
	default:
		fmt.Fprintf(&b, "### 🟢 Idle\n\n")
	}

	via := ""
	if state.Blocked.Source != "" && state.Blocked.Source != "init" {
		via = fmt.Sprintf("  _(via %s)_", state.Blocked.Source)
	}
	remaining := "available now"
	if blocked {
		remaining = "0 — rate-limited"
	}

	fmt.Fprintf(&b, "|   |   |\n|---|---|\n")
	fmt.Fprintf(&b, "| **Scope** | `%s` |\n", state.Blocked.Scope)
	fmt.Fprintf(&b, "| **Reviews remaining** | %s%s |\n", remaining, via)
	if blocked {
		fmt.Fprintf(&b, "| **Rate limit** | ⚠️ rate limited |\n")
	} else {
		fmt.Fprintf(&b, "| **Rate limit** | ✅ not currently limited |\n")
	}
	fmt.Fprintf(&b, "| **Last review fired** | %s |\n", fmtStamp(state.LastFired, loc))
	if state.InFlight != nil {
		fmt.Fprintf(&b, "| **In flight** | [%s#%d](https://github.com/%s/pull/%d) · fired %s · `%s` |\n",
			state.InFlight.Repo, state.InFlight.PR, state.InFlight.Repo, state.InFlight.PR,
			fmtStamp(state.InFlight.FiredAt, loc), state.InFlight.ByHost)
	} else {
		fmt.Fprintf(&b, "| **In flight** | — |\n")
	}
	if len(awaiting) > 0 {
		wait := awaiting[0]
		fmt.Fprintf(&b, "| **Feedback wait** | [%s#%d](https://github.com/%s/pull/%d) · `%s` · deadline %s |\n",
			wait.Repo, wait.PR, wait.Repo, wait.PR, wait.Head, fmtStamp(&wait.Deadline, loc))
	} else {
		fmt.Fprintf(&b, "| **Feedback wait** | — |\n")
	}
	if state.Warn != "" {
		fmt.Fprintf(&b, "\n> ⚠️ %s\n", state.Warn)
	}

	fmt.Fprintf(&b, "\n## ⏳ Queue — %d waiting\n\n", len(queue))
	if len(queue) == 0 {
		fmt.Fprintf(&b, "_Nothing queued._\n")
	} else {
		fmt.Fprintf(&b, "| # | PR | enqueued | host |\n|--:|---|---|---|\n")
		for i, item := range queue {
			fmt.Fprintf(&b, "| %d | [%s#%d](https://github.com/%s/pull/%d) | %s | `%s` |\n",
				i+1, item.Repo, item.PR, item.Repo, item.PR, fmtStamp(&item.EnqueuedAt, loc), item.Host)
		}
	}

	fmt.Fprintf(&b, "\n## ✅ Recently reviewed — last %d\n\n", len(state.History))
	if len(state.History) == 0 {
		fmt.Fprintf(&b, "_None yet._\n")
	} else {
		fmt.Fprintf(&b, "| PR | commit | reviewed | host |\n|---|---|---|---|\n")
		for _, item := range state.History {
			fmt.Fprintf(&b, "| [%s#%d](https://github.com/%s/pull/%d) | `%s` | %s | `%s` |\n",
				item.Repo, item.PR, item.Repo, item.PR, item.Commit, fmtStamp(&item.At, loc), item.Host)
		}
	}

	fmt.Fprintf(&b, "\n---\n")
	fmt.Fprintf(&b, "<sub>🤖 Managed by [crq](%s) · rev %d · updated %s · do not edit by hand (machine state is in the hidden block at the top).</sub>\n",
		crqProjectURL, state.Rev, fmtStamp(state.UpdatedAt, loc))
	return b.String()
}

func renderTitle(state State) string {
	now := time.Now().UTC()
	switch {
	case state.Blocked.BlockedUntil != nil && state.Blocked.BlockedUntil.After(now):
		return fmt.Sprintf("🐰 crq — blocked · queue %d", len(state.Queue))
	case state.InFlight != nil:
		return fmt.Sprintf("🐰 crq — reviewing #%d · queue %d", state.InFlight.PR, len(state.Queue))
	case len(state.AwaitingFeedback) > 0:
		return fmt.Sprintf("🐰 crq — awaiting feedback · queue %d", len(state.Queue))
	case len(state.Queue) > 0:
		return fmt.Sprintf("🐰 crq — %d queued", len(state.Queue))
	default:
		return "🐰 crq — idle"
	}
}

func issueBody(state State, cfg Config) (string, error) {
	machine, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s\n%s\n%s\n\n%s", stateBegin, machine, stateEnd, renderDashboard(state, cfg)), nil
}

func hashString(value string) string {
	sum := sha1.Sum([]byte(value))
	return hex.EncodeToString(sum[:])
}
