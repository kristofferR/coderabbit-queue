// Package state defines crq's persisted schema v3: one Round per tracked PR,
// a single global fire slot, and the CodeRabbit account quota. A Round is
// never deleted, only transitioned (or archived when superseded by a new
// head) — the invariant that makes "forgot we already requested a review at
// this head" unrepresentable. That amnesia — a rate-limited requeue deleting
// the fired marker — is what let the daemon spam `@coderabbitai review` 19
// times on one PR in a day.
package state

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Phase is a Round's position in its lifecycle. Legal transitions are owned
// by the methods on Round; everything else must go through them.
//
//	queued → reserved → fired → reviewing → completed
//	   ↑         │         │         │
//	   └─────────┘         ├─────────┴→ awaiting_retry (→ fire-eligible again)
//	 (post failed)         └→ completed (review lands while slot held)
//	 any → abandoned (PR closed, cancelled, or superseded by a new head)
type Phase string

const (
	PhaseQueued        Phase = "queued"         // waiting for a fire slot
	PhaseReserved      Phase = "reserved"       // slot held, command not yet posted
	PhaseFired         Phase = "fired"          // command posted (or adopted), review pending
	PhaseReviewing     Phase = "reviewing"      // bot acknowledged; slot released, review runs
	PhaseAwaitingRetry Phase = "awaiting_retry" // throttled or timed out; may re-fire at RetryAt
	PhaseCompleted     Phase = "completed"      // every required bot reviewed this head
	PhaseAbandoned     Phase = "abandoned"      // closed, cancelled, or superseded
)

// Round is one review cycle for a repo#pr at a specific head. RetryAt is the
// per-head cooldown that survives every transition: an awaiting_retry round
// refuses to fire again before it, no matter how many daemon passes observe
// "no bot review at head" in the meantime.
type Round struct {
	Repo     string `json:"repo"`
	PR       int    `json:"pr"`
	Head     string `json:"head"` // 9-char short SHA
	Seq      int64  `json:"seq"`
	Phase    Phase  `json:"phase"`
	Attempts int    `json:"attempts,omitempty"` // fire attempts for this head

	EnqueuedAt time.Time  `json:"enqueued_at"`
	ReservedAt *time.Time `json:"reserved_at,omitempty"`
	FiredAt    *time.Time `json:"fired_at,omitempty"`

	// CommandID is the review-command comment that fired this round (posted or
	// adopted). It anchors completion-reply pairing to this round.
	CommandID int64 `json:"command_id,omitempty"`

	// CodexCommandID is the `@codex review` comment crq posted/adopted for this
	// round (0 when Codex was not fired). It suppresses re-posting the Codex
	// command on a retry of the same head.
	//
	// Legacy twin of CoBots["chatgpt-codex-connector"].CommandID: the fleet
	// shares one state ref across binary versions, so Codex bookkeeping is
	// dual-written (accessors mirror into these fields, Normalize folds them
	// back into CoBots) — old binaries must keep seeing codex_command_id.
	CodexCommandID int64 `json:"codex_command_id,omitempty"`

	// CoBots is the per-round co-reviewer bookkeeping, keyed by normalized
	// login (no "[bot]" suffix). Bots other than Codex exist only here — old
	// binaries never fired them, so ignoring the map is correct for them.
	// Mutate only through Co/SetCoCommand/ClaimCo/ClearCoClaim.
	CoBots map[string]CoBotRound `json:"cobots,omitempty"`

	// CoOnly marks a round that reached its fired/reviewing state WITHOUT crq
	// requesting a primary review — a co-reviewer-only trigger, or a bounded
	// co-review wait. FiredAt does double duty as the evidence-floor anchor and
	// as "we asked for a review", and those two meanings diverged the moment
	// co-reviewer-only rounds existed: the dashboard's requested-reviews table
	// filled with repos crq had never asked CodeRabbit about. Anything counting
	// CodeRabbit requests must skip these.
	CoOnly bool `json:"co_only,omitempty"`

	// RetryAt is the earliest time this head may fire again (awaiting_retry).
	RetryAt *time.Time `json:"retry_at,omitempty"`

	// CodexClaimedAt reserves the self-heal Codex post: it is CAS-set before the
	// network post so two unserialized sweepers cannot both post `@codex review`
	// for the same round. A stale claim (the poster died mid-flight) expires and
	// may be re-claimed.
	CodexClaimedAt *time.Time `json:"codex_claimed_at,omitempty"`
	// CodexCommandedAt records when crq posted this round's Codex command. It
	// can precede FiredAt (a deferred post while queued behind a rate-limit
	// window or busy slot), and Codex evidence binds from it — otherwise a
	// SHA-less Codex answer delivered before the delayed CodeRabbit fire
	// would be ignored by the completion cutoff and never re-requested.
	CodexCommandedAt *time.Time `json:"codex_commanded_at,omitempty"`

	// LastAttemptAt is the adoption cutoff: command comments older than the
	// most recent failed/abandoned attempt must not be adopted as this round's
	// fire.
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`

	// WaitDeadline bounds how long a fired/reviewing round is waited on before
	// the round is retried or surfaced as timed out.
	WaitDeadline *time.Time `json:"wait_deadline,omitempty"`

	// ReviewersChanged marks a completed round whose effective reviewer set
	// changed while its pull request was closed. Requeueing a closed PR's round
	// would hand Pump dead work ahead of every live round, so the change is
	// recorded on the marker instead: if the PR is ever reopened, enqueue reopens
	// the round rather than treating it as "this head was reviewed". Reopen
	// clears it — the reopened round answers under the current requirements.
	ReviewersChanged bool `json:"reviewers_changed,omitempty"`

	Token string `json:"token,omitempty"` // reservation token (CAS race detection)
	// ByHost identifies the PROCESS that reserved this round, in the writer form
	// "host=<name> pid=<n> run=<id>" — the key NoteWriter records capabilities under, so
	// LaggingWriters can ask whether the process driving a fire understands the
	// configuration it is firing from. The dashboard shows the machine name.
	ByHost string `json:"by_host,omitempty"`
	Note   string `json:"note,omitempty"` // human-readable reason for the last transition

	// unknown carries JSON members this binary has no field for, so a newer
	// binary's additions survive being read and rewritten here. Unexported, so
	// it is never a member itself. See tolerant.go.
	unknown unknownFields
}

// CoBotRound is one co-reviewer's bookkeeping inside a Round: the trigger
// comment crq posted/adopted for it, and the CAS claim that serializes the
// pending post (the per-round concurrency guard — co-reviewer fires never
// take the global FireSlot).
type CoBotRound struct {
	CommandID   int64      `json:"command_id,omitempty"`
	CommandedAt *time.Time `json:"commanded_at,omitempty"`
	ClaimedAt   *time.Time `json:"claimed_at,omitempty"`
}

func (c CoBotRound) empty() bool {
	return c.CommandID == 0 && c.CommandedAt == nil && c.ClaimedAt == nil
}

// codexCoBotKey is dialect.CodexBotLogin under coBotKey. The literal is
// repeated here because state stays stdlib-only; state_test pins the two in
// sync.
const codexCoBotKey = "chatgpt-codex-connector"

// coBotKey mirrors dialect.NormalizeBotName: CoBots keys carry no "[bot]"
// suffix regardless of which login spelling the caller observed.
func coBotKey(login string) string { return strings.TrimSuffix(login, "[bot]") }

// Co returns login's bookkeeping for this round (zero value when none).
func (r *Round) Co(login string) CoBotRound {
	return r.CoBots[coBotKey(login)]
}

// setCo stores login's entry copy-on-write (Rounds are copied by value while
// the map inside would otherwise be shared) and dual-writes Codex's entry
// into the legacy fields old binaries read.
func (r *Round) setCo(login string, c CoBotRound) {
	key := coBotKey(login)
	m := make(map[string]CoBotRound, len(r.CoBots)+1)
	for k, v := range r.CoBots {
		m[k] = v
	}
	if c.empty() {
		delete(m, key)
	} else {
		m[key] = c
	}
	if len(m) == 0 {
		m = nil
	}
	r.CoBots = m
	if key == codexCoBotKey {
		r.CodexCommandID = c.CommandID
		r.CodexCommandedAt = c.CommandedAt
		r.CodexClaimedAt = c.ClaimedAt
	}
}

// SetCoCommand records the trigger comment posted/adopted for login this
// round and releases its claim.
func (r *Round) SetCoCommand(login string, commandID int64, at time.Time) {
	c := r.Co(login)
	c.CommandID = commandID
	t := at.UTC()
	c.CommandedAt = &t
	c.ClaimedAt = nil
	r.setCo(login, c)
}

// ClaimCo reserves the pending trigger post for login: CAS-set before the
// network post so two unserialized sweepers cannot both post the command for
// the same round. A stale claim (the poster died mid-flight) expires by age
// and may be re-claimed.
func (r *Round) ClaimCo(login string, now time.Time) {
	c := r.Co(login)
	t := now.UTC()
	c.ClaimedAt = &t
	r.setCo(login, c)
}

// ClearCoClaim releases login's claim without recording a command.
func (r *Round) ClearCoClaim(login string) {
	c := r.Co(login)
	c.ClaimedAt = nil
	r.setCo(login, c)
}

// foldLegacyCodex folds the legacy per-round Codex fields into CoBots on
// load. Every writer keeps the legacy fields current for Codex (old binaries
// write only them, new ones dual-write), so they are authoritative in BOTH
// directions: they overwrite the mirror entry, and an empty legacy set clears
// a stale mirror (a writer zeroed the legacy fields directly).
func (r *Round) foldLegacyCodex() {
	r.setCo(codexCoBotKey, CoBotRound{CommandID: r.CodexCommandID, CommandedAt: r.CodexCommandedAt, ClaimedAt: r.CodexClaimedAt})
}

// inferCoOnly backfills CoOnly for rounds written before the flag existed. The
// evidence is already in the round: a co-reviewer-only fire recorded one of its
// OWN trigger comments as the round's CommandID, whereas a real fire records
// the primary review command — two different comments, so the inference cannot
// mislabel a genuine request.
func (r *Round) inferCoOnly() {
	if r.CoOnly || r.FiredAt == nil {
		return
	}
	// A bounded co-review wait anchors FiredAt without ever posting a command,
	// so a fire with no CommandID is one by construction: every real fire
	// records the command it posted or adopted.
	if r.CommandID == 0 {
		r.CoOnly = true
		return
	}
	// A co-reviewer-only fire recorded one of its OWN trigger comments as the
	// round's CommandID; a real fire records the primary review command, a
	// different comment, so this cannot mislabel a genuine request.
	for _, c := range r.CoBots {
		if c.CommandID == r.CommandID {
			r.CoOnly = true
			return
		}
	}
}

// FireSlot is the single global in-flight reservation: at most one review
// command may be getting posted at a time, fleet-wide.
type FireSlot struct {
	Key   string    `json:"key"` // repo#pr holding the slot
	Token string    `json:"token"`
	Since time.Time `json:"since"`
	// HoldUntil keeps the slot taken after the round holding it went away with
	// its metered command still unacknowledged — a head advance archives the
	// round, and the command it posted does not stop being in flight because of
	// that. Set to the end of that command's in-flight window, so the hold is
	// bounded by the same deadline Progress would have applied.
	HoldUntil *time.Time `json:"hold_until,omitempty"`

	// unknown carries JSON members this binary has no field for, so a newer
	// binary's additions survive being read and rewritten here. See tolerant.go.
	unknown unknownFields
}

// AccountQuota is the CodeRabbit account-wide review quota (NOT the GitHub
// REST quota — that is internal/gh's Throttle). Set only from classified
// CodeRabbit comments.
type AccountQuota struct {
	Scope        string     `json:"scope,omitempty"`
	BlockedUntil *time.Time `json:"blocked_until,omitempty"`
	Remaining    *int       `json:"remaining,omitempty"`
	Source       string     `json:"source,omitempty"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
	CalibAskedAt *time.Time `json:"calib_asked_at,omitempty"`
	// RLCommentID/RLCommentUpdated identify the rate-limit comment whose "next
	// review available in" window produced the current block. CodeRabbit edits a
	// single rate-limit comment in place instead of posting a new one, so its
	// UpdatedAt advances past every later fire; tracking it lets a re-observed
	// edit reuse the standing block instead of being counted as a fresh event
	// that extends the window on every bounce.
	RLCommentID      int64      `json:"rl_comment_id,omitempty"`
	RLCommentUpdated *time.Time `json:"rl_comment_updated,omitempty"`
}

type LeaderLease struct {
	Owner     string    `json:"owner"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// State is schema v3. It persists as state.json in the git state ref exactly
// like v2; only the payload shape changed (no migration — v2 payloads
// auto-reinit, crq is pre-release).
type State struct {
	Version int   `json:"v"` // 3
	Rev     int64 `json:"rev"`
	NextSeq int64 `json:"next_seq"`

	Rounds    map[string]Round `json:"rounds"`
	FireSlot  *FireSlot        `json:"fire_slot,omitempty"`
	LastFired *time.Time       `json:"last_fired,omitempty"`
	Account   AccountQuota     `json:"account"`
	Leader    *LeaderLease     `json:"leader,omitempty"`

	// CalibrationIssue overrides the configured calibration PR/issue when the
	// original hit GitHub's hard 2500-comment cap and crq rotated to a fresh
	// one. Persisted in the shared state so the whole fleet uses the new issue.
	CalibrationIssue int `json:"calibration_issue,omitempty"`

	// Writers records which hosts have written this state and what they can do,
	// so a feature that only SOME binaries understand can say so instead of
	// pretending agreement. Sharing a ref stops an old binary erasing a new
	// field; it does not make that binary act on it.
	Writers map[string]WriterSeen `json:"writers,omitempty"`

	// Repos holds per-repository reviewer overrides, keyed by normalized
	// "owner/name".
	//
	// It lives HERE, in the shared state ref, rather than in a .crq.yaml the
	// repository carries. The daemon has no checkout of any repository it
	// reviews, so an in-repo file would be invisible to it or cost a REST fetch
	// per PR — and a daemon and an agent reading different configurations while
	// writing one shared state ref is a new class of divergence. Both already
	// read this ref, so both cannot disagree about it.
	Repos map[string]RepoReviewers `json:"repos,omitempty"`

	// Archive keeps recently finished rounds (superseded, closed, cancelled)
	// for the dashboard and debugging. Bounded by ArchiveMax.
	Archive []Round `json:"archive,omitempty"`

	Warn         string     `json:"warn,omitempty"`
	UpdatedAt    *time.Time `json:"wrote_at,omitempty"`
	DashboardSHA string     `json:"dashboard_sha,omitempty"`

	// unknown carries top-level JSON members this binary has no field for. See
	// tolerant.go.
	unknown unknownFields
}

// WriterCaps is what THIS binary understands. Bump it when a state field starts
// changing decisions, so a fleet running two versions can tell.
const WriterCaps = 1

// CapsRepoOverrides is the capability that makes per-repository reviewer
// overrides safe to act on.
const CapsRepoOverrides = 1

// writerTTL is how long a host counts as still active for capability purposes.
const writerTTL = 30 * time.Minute

// WriterSeen is one host's last write.
type WriterSeen struct {
	Caps int       `json:"caps"`
	At   time.Time `json:"at"`
}

// NoteWriter records that a process wrote this state with the given
// capabilities. The key identifies the PROCESS, not the machine: a new CLI and
// an old daemon on one host is the ordinary upgrade, and keying by hostname
// would let the CLI's write vouch for the daemon that has not been upgraded.
func (s *State) NoteWriter(host string, caps int, now time.Time) {
	if host == "" {
		return
	}
	if s.Writers == nil {
		s.Writers = map[string]WriterSeen{}
	}
	s.Writers[host] = WriterSeen{Caps: caps, At: now.UTC()}
	// Bounded: a host that has not written in a day is not part of the fleet's
	// current behaviour, and the state ref is not an audit log.
	for name, seen := range s.Writers {
		if now.Sub(seen.At) > 24*time.Hour {
			delete(s.Writers, name)
		}
	}
}

// LaggingWriters names the hosts that are DRIVING this queue — holding the
// leader lease or the fire slot — without having announced the capability
// needed for caps.
//
// It answers the question a shared config field cannot: "will everyone who acts
// on this actually honour it?" An old binary loads an unknown field, writes it
// back untouched, and keeps deciding from its own fleet-wide configuration.
func (s *State) LaggingWriters(caps int, now time.Time) []string {
	acting := map[string]bool{}
	// The leader identifies itself as "host=<name> pid=<n> run=<id>", which is
	// exactly the process identity capabilities are recorded under — the run
	// component is what keeps a restart into a reused pid from inheriting them.
	if s.Leader != nil && s.Leader.ExpiresAt.After(now) && strings.TrimSpace(s.Leader.Owner) != "" {
		acting[s.Leader.Owner] = true
	}
	if slot := s.SlotRound(); slot != nil && slot.ByHost != "" {
		acting[slot.ByHost] = true
	}
	var out []string
	for host := range acting {
		if seen, ok := s.Writers[host]; ok && seen.Caps >= caps && now.Sub(seen.At) <= writerTTL {
			continue
		}
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}

// RepoReviewers overrides which reviewers run on one repository. A nil slice
// means "no override, use the fleet default"; an empty non-nil slice means
// "none here" — the difference is why these are pointers to slices in JSON
// terms and why SetRepoReviewers takes explicit values.
type RepoReviewers struct {
	// CoBots are the co-reviewer logins enabled here, replacing the fleet list.
	CoBots []string `json:"cobots,omitempty"`
	// Required are the logins that gate convergence here.
	Required []string `json:"required,omitempty"`
	// SetCoBots/SetRequired record whether each list was set at all, so
	// "explicitly none" survives a JSON round trip that drops empty slices.
	SetCoBots   bool       `json:"set_cobots,omitempty"`
	SetRequired bool       `json:"set_required,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
	By          string     `json:"by,omitempty"`
}

// RepoOverride returns the override for repo, and whether one exists.
func (s *State) RepoOverride(repo string) (RepoReviewers, bool) {
	ov, ok := s.Repos[normalizeRepoKey(repo)]
	return ov, ok
}

// SetRepoOverride records repo's reviewer override, replacing any earlier one.
func (s *State) SetRepoOverride(repo string, ov RepoReviewers) {
	if s.Repos == nil {
		s.Repos = map[string]RepoReviewers{}
	}
	s.Repos[normalizeRepoKey(repo)] = ov
}

// ClearRepoOverride drops repo's override, returning it to the fleet default.
func (s *State) ClearRepoOverride(repo string) bool {
	key := normalizeRepoKey(repo)
	if _, ok := s.Repos[key]; !ok {
		return false
	}
	delete(s.Repos, key)
	return true
}

func normalizeRepoKey(repo string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(repo), ".git"))
}

const SchemaVersion = 3

// ArchiveMax bounds the finished-rounds ring. Active rounds are never
// evicted — only Archive is trimmed — so a live "already fired at this head"
// marker cannot be lost to an eviction cap.
const ArchiveMax = 50

func Key(repo string, pr int) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(repo), pr)
}

func New() State {
	return State{Version: SchemaVersion, Rounds: map[string]Round{}}
}

// --- Round transitions -----------------------------------------------------

type TransitionError struct {
	From, To Phase
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("illegal round transition %s → %s", e.From, e.To)
}

func (r *Round) illegal(to Phase) error { return &TransitionError{From: r.Phase, To: to} }

// Reserve takes the fire slot for this round: queued (or retry-eligible
// awaiting_retry) → reserved. writer is the reserving process's writer id (see
// ByHost), not a bare hostname.
func (r *Round) Reserve(token, writer string, now time.Time) error {
	if r.Phase != PhaseQueued && !r.retryEligible(now) {
		return r.illegal(PhaseReserved)
	}
	r.Phase = PhaseReserved
	r.Token = token
	r.ByHost = writer
	t := now.UTC()
	r.ReservedAt = &t
	r.Note = ""
	return nil
}

// Fire records the posted (or adopted) review command: reserved → fired.
// Adoption of an already-posted command fires straight from queued.
func (r *Round) Fire(commandID int64, at time.Time) error {
	if r.Phase != PhaseReserved && r.Phase != PhaseQueued {
		return r.illegal(PhaseFired)
	}
	r.Phase = PhaseFired
	r.CommandID = commandID
	t := at.UTC()
	r.FiredAt = &t
	r.Attempts++
	r.Note = ""
	return nil
}

// ReleaseToQueue returns a reservation that never posted: reserved → queued.
// The attempt still counts and LastAttemptAt moves, so a stale command comment
// from before the failure cannot be adopted later.
func (r *Round) ReleaseToQueue(reason string, now time.Time) error {
	if r.Phase != PhaseReserved {
		return r.illegal(PhaseQueued)
	}
	r.Phase = PhaseQueued
	r.Token = ""
	r.ReservedAt = nil
	r.Attempts++
	t := now.UTC()
	r.LastAttemptAt = &t
	r.Note = reason
	return nil
}

// Reopen puts a completed round back in the queue because its effective
// reviewer set changed.
//
// A completed round is the "this head was reviewed" dedup marker, so a newly
// required reviewer would otherwise strand the PR: convergence reports it
// pending while enqueue keeps skipping the head, and no eligible round exists to
// trigger it. An optional reviewer also needs an active round for its trigger,
// self-heal and bounded participation wait. This is the one transition that
// reopens a finished round, and it keeps the head, the attempts and the
// co-reviewer bookkeeping — what changed is who runs, not what happened.
//
// LastAttemptAt is deliberately left alone: it is the adoption floor for a
// FAILED attempt, and moving it would discard a newly required co-reviewer's own
// unanswered trigger comment as too old to adopt — so crq would post that bot a
// second request for the very round the reopen exists to let it answer.
func (r *Round) Reopen() error {
	if r.Phase != PhaseCompleted {
		return r.illegal(PhaseQueued)
	}
	r.Phase = PhaseQueued
	r.Token = ""
	r.ReservedAt = nil
	r.WaitDeadline = nil
	r.RetryAt = nil
	r.ReviewersChanged = false
	r.Note = "reviewer configuration changed"
	return nil
}

// Acknowledge records that the bot has seen the fired command (reaction,
// in-progress summary, or other non-terminal reply): fired → reviewing. The
// fire slot may be released; the round itself stays open until Complete.
func (r *Round) Acknowledge() error {
	if r.Phase == PhaseReviewing {
		return nil // idempotent: acks arrive repeatedly while a review runs
	}
	if r.Phase != PhaseFired {
		return r.illegal(PhaseReviewing)
	}
	r.Phase = PhaseReviewing
	r.Note = ""
	return nil
}

// AwaitRetry parks the round until retryAt: fired|reviewing|reserved →
// awaiting_retry. This REPLACES the v2 "delete the fired marker and requeue"
// path — the round keeps its head, attempts, and fire history, so the next
// daemon pass sees "already requested, waiting" instead of "never fired".
func (r *Round) AwaitRetry(retryAt time.Time, reason string, now time.Time) error {
	switch r.Phase {
	case PhaseFired, PhaseReviewing, PhaseReserved:
	default:
		return r.illegal(PhaseAwaitingRetry)
	}
	r.Phase = PhaseAwaitingRetry
	t := retryAt.UTC()
	r.RetryAt = &t
	n := now.UTC()
	r.LastAttemptAt = &n
	r.Token = ""
	r.ReservedAt = nil
	r.Note = reason
	return nil
}

// AwaitCoReview bounds a co-review wait: the configured primary bot already
// reviewed the head, but a gating co-bot (Codex) has not — so crq waits for it,
// bounded by deadline, WITHOUT posting a command or holding the fire slot. Legal
// from queued|awaiting_retry|fired|reviewing → reviewing. FiredAt is the wait
// anchor: the primary review already stands in as the fire, so it is set to now
// only when no fire was recorded. Token/ReservedAt are cleared (no slot is held);
// CodexCommandID is left as-is so an existing Codex command is not re-posted.
func (r *Round) AwaitCoReview(deadline, anchor time.Time) error {
	switch r.Phase {
	case PhaseQueued, PhaseAwaitingRetry, PhaseFired, PhaseReviewing:
	default:
		return r.illegal(PhaseReviewing)
	}
	r.Phase = PhaseReviewing
	dl := deadline.UTC()
	r.WaitDeadline = &dl
	if r.FiredAt == nil {
		// The anchor is the wait's evidence floor (Completion ignores SHA-less
		// co-bot summaries before FiredAt). Callers pass the adopted co-bot
		// command's time when one exists — anchoring at observation time would
		// hide an answer posted between that command and this pump.
		t := anchor.UTC()
		r.FiredAt = &t
	}
	r.Token = ""
	r.ReservedAt = nil
	r.Note = "awaiting codex co-review"
	return nil
}

// Complete finishes the round: fired|reviewing → completed. A completed round
// stays in Rounds (it IS the "this head was reviewed" dedup marker) until a
// new head supersedes it or the PR closes.
func (r *Round) Complete() error {
	if r.Phase != PhaseFired && r.Phase != PhaseReviewing {
		return r.illegal(PhaseCompleted)
	}
	r.Phase = PhaseCompleted
	r.Note = ""
	return nil
}

// Dedupe completes a not-yet-fired round because the configured bot already
// reviewed its head independently (an adopted review, not a fire crq made): a
// queued (or retry-eligible) round → completed. The completed round stays as
// the "this head was reviewed" dedup marker without recording a fictitious
// fire (FiredAt stays nil).
func (r *Round) Dedupe(now time.Time) error {
	if r.Phase != PhaseQueued && r.Phase != PhaseReserved && !r.retryEligible(now) {
		return r.illegal(PhaseCompleted)
	}
	r.Phase = PhaseCompleted
	r.Token = ""
	r.ReservedAt = nil
	r.Note = "bot already reviewed head"
	return nil
}

// Abandon ends the round from any phase (PR closed/merged, cancelled, or
// superseded by a new head). The caller archives it via State.EndRound.
func (r *Round) Abandon(reason string) {
	r.Phase = PhaseAbandoned
	r.Token = ""
	r.Note = reason
}

func (r *Round) retryEligible(now time.Time) bool {
	return r.Phase == PhaseAwaitingRetry && r.RetryAt != nil && !now.Before(*r.RetryAt)
}

// FireEligible reports whether Pump may consider this round for firing now.
func (r *Round) FireEligible(now time.Time) bool {
	return r.Phase == PhaseQueued || r.retryEligible(now)
}

// Active reports whether the round still occupies its PR slot (i.e. is not
// finished). Completed rounds are NOT active but still occupy Rounds as the
// reviewed-head marker.
func (r *Round) Active() bool {
	switch r.Phase {
	case PhaseQueued, PhaseReserved, PhaseFired, PhaseReviewing, PhaseAwaitingRetry:
		return true
	}
	return false
}

// --- State operations ------------------------------------------------------

// Round returns the current round for repo#pr, or nil.
func (s *State) Round(repo string, pr int) *Round {
	if s.Rounds == nil {
		return nil
	}
	r, ok := s.Rounds[Key(repo, pr)]
	if !ok {
		return nil
	}
	return &r
}

// PutRound stores r as the current round for its PR.
func (s *State) PutRound(r Round) {
	if s.Rounds == nil {
		s.Rounds = map[string]Round{}
	}
	s.Rounds[Key(r.Repo, r.PR)] = r
}

// NewRound begins a round for a head with no current round. It refuses to
// clobber an existing round — supersede via EndRound first — so "two rounds
// for one PR" cannot happen by accident.
func (s *State) NewRound(repo string, pr int, head string, now time.Time) (*Round, error) {
	key := Key(repo, pr)
	if s.Rounds == nil {
		s.Rounds = map[string]Round{}
	}
	if cur, ok := s.Rounds[key]; ok {
		return nil, fmt.Errorf("round already exists for %s@%s (%s)", key, cur.Head, cur.Phase)
	}
	s.NextSeq++
	r := Round{
		Repo:       strings.ToLower(repo),
		PR:         pr,
		Head:       head,
		Seq:        s.NextSeq,
		Phase:      PhaseQueued,
		EnqueuedAt: now.UTC(),
	}
	s.Rounds[key] = r
	return &r, nil
}

// EndRound abandons the current round (superseded/closed/cancelled) and moves
// it to the archive. The PR has no round afterwards.
func (s *State) EndRound(repo string, pr int, reason string) {
	key := Key(repo, pr)
	r, ok := s.Rounds[key]
	if !ok {
		return
	}
	r.Abandon(reason)
	delete(s.Rounds, key)
	s.Archive = append(s.Archive, r)
	if len(s.Archive) > ArchiveMax {
		s.Archive = s.Archive[len(s.Archive)-ArchiveMax:]
	}
}

// Supersede replaces the round for repo#pr with a fresh queued round at the
// new head, archiving the old one. It is the ONLY way a round's head changes.
func (s *State) Supersede(repo string, pr int, head string, now time.Time) (*Round, error) {
	s.EndRound(repo, pr, "superseded by "+head)
	return s.NewRound(repo, pr, head, now)
}

// SlotRound returns the round currently holding the fire slot, or nil. A slot
// whose round vanished or moved on is stale and is reported as nil (the
// caller clears it).
func (s *State) SlotRound() *Round {
	if s.FireSlot == nil {
		return nil
	}
	r, ok := s.Rounds[s.FireSlot.Key]
	if !ok || (r.Phase != PhaseReserved && r.Phase != PhaseFired) || r.Token != s.FireSlot.Token {
		return nil
	}
	return &r
}

// SlotHeld reports whether the fire slot is taken — the question every fire gate
// actually asks. A live round holds it; so does an orphaned hold, left behind
// when the round that posted a still-unacknowledged metered command was
// superseded by a new head. Without the second case the expected push after a
// round that converged without its primary would free the slot for a second
// concurrent metered command.
func (s *State) SlotHeld(now time.Time) bool {
	if s.FireSlot == nil {
		return false
	}
	if s.SlotRound() != nil {
		return true
	}
	return s.FireSlot.HoldUntil != nil && s.FireSlot.HoldUntil.After(now)
}

// HoldSlotUntil keeps the current fire slot held past the round that owns it.
// The caller sets the deadline, since only it knows the in-flight window the
// command this slot was taken for is bounded by.
func (s *State) HoldSlotUntil(until time.Time) {
	if s.FireSlot == nil {
		return
	}
	u := until.UTC()
	s.FireSlot.HoldUntil = &u
}

// NextEligible returns the fire-eligible round with the lowest Seq, or nil.
func (s *State) NextEligible(now time.Time) *Round {
	var best *Round
	for key := range s.Rounds {
		r := s.Rounds[key]
		if !r.FireEligible(now) {
			continue
		}
		if best == nil || r.Seq < best.Seq {
			c := r
			best = &c
		}
	}
	return best
}

// QueuedRounds returns every fire-eligible round ordered by Seq (dashboard).
func (s *State) QueuedRounds(now time.Time) []Round {
	var out []Round
	for _, r := range s.Rounds {
		if r.FireEligible(now) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Queue-entry wait reasons. A waiting round is held by exactly one of these
// (or by nothing, in which case it is next up).
const (
	WaitCoolingDown    = "cooling down"    // this round's own RetryAt has not passed
	WaitAccountBlocked = "account blocked" // the CodeRabbit account quota window dominates
	WaitSlotBusy       = "slot busy"       // another PR's review holds the fire slot
	WaitPacing         = "pacing"          // LastFired + CRQ_MIN_INTERVAL has not passed
	WaitBehind         = "behind an earlier round"
)

// QueueEntry is one round waiting for a fire, plus when it can actually fire
// and what is holding it. It is a VIEW over Rounds + Account — never persisted,
// so it carries no schema obligations.
type QueueEntry struct {
	Round
	// ReadyAt is the earliest time this round may fire. Zero means "now".
	ReadyAt time.Time
	// Why is the gate holding it: one of the Wait* constants, or "" when the
	// round is simply next up.
	Why string
}

// roundReadyAt is a waiting round's OWN not-before time: RetryAt while it is
// cooling down, zero (ready now) while it is merely queued.
func roundReadyAt(r Round) time.Time {
	if r.Phase == PhaseAwaitingRetry && r.RetryAt != nil {
		return r.RetryAt.UTC()
	}
	return time.Time{}
}

// Queue returns every round waiting for a fire — queued AND awaiting_retry — in
// the order they will actually reach the slot: by ready time, then by Seq.
//
// There is only one queue. A cooling-down round is not a different species of
// work, it is a queued round with a not-before time, and rendering it as a
// separate list left "nothing queued" and "two PRs parked until 00:07Z" looking
// identical. Ordering by (ReadyAt, Seq) reproduces what firing actually does: a
// round whose window has not opened cannot precede a ready one, and among ready
// rounds NextEligible takes the lowest Seq.
//
// ReadyAt folds in the account-wide quota block, because DecideFire gates on it
// too — showing a round's own RetryAt alone promises a time the fire gate will
// not honour once CodeRabbit extends the window. This is the same max() that
// AccountBlockedUntil computes for the wait path.
//
// minInterval is folded in too. It is DecideFire's pacing gate, so leaving it out
// rendered a round "ready: now" that firing would refuse for up to another
// CRQ_MIN_INTERVAL — 90s by default and configurable far longer. It is an
// absolute boundary (LastFired + minInterval), not a countdown, so surfacing it
// does not churn DashboardSHA between renders.
func (s *State) Queue(now time.Time, minInterval time.Duration) []QueueEntry {
	var blocked time.Time
	if s.Account.BlockedUntil != nil && s.Account.BlockedUntil.After(now) {
		blocked = s.Account.BlockedUntil.UTC()
	}
	slotBusy := s.SlotHeld(now)
	// The pacing gate applies to whichever round fires next, so it bounds every
	// entry's earliest possible start.
	var paced time.Time
	if minInterval > 0 && s.LastFired != nil {
		if at := s.LastFired.Add(minInterval).UTC(); at.After(now) {
			paced = at
		}
	}

	// Split by whether the round is waiting for anything the queue serializes.
	//
	// A co-only round is not. It spends no account quota, takes no fire slot, and
	// DecideFire resolves it before either gate — so the account window, the slot,
	// and its position behind other rounds are all irrelevant to it. Exempting it
	// gate-by-gate was tried and missed a different spot three times running; the
	// partition makes the exemption structural, so a gate added later cannot
	// silently apply to work it does not govern.
	var queued, freeRunning []QueueEntry
	for _, r := range s.Rounds {
		if r.Phase != PhaseQueued && r.Phase != PhaseAwaitingRetry {
			continue
		}
		e := QueueEntry{Round: r}
		// Its own cooldown binds either way: that is the round's, not the queue's.
		if own := roundReadyAt(r); own.After(now) {
			e.ReadyAt, e.Why = own, WaitCoolingDown
		}
		if r.CoOnly {
			freeRunning = append(freeRunning, e)
			continue
		}
		// Every remaining gate is a lower bound on when firing will accept this
		// round, so the ready time is the LATEST of them and the reason is
		// whichever binds. Taking the first that matched let a one-minute cooldown
		// hide a two-hour account block.
		for _, g := range []struct {
			at  time.Time
			why string
		}{{blocked, WaitAccountBlocked}, {paced, WaitPacing}} {
			if g.at.After(now) && g.at.After(e.ReadyAt) {
				e.ReadyAt, e.Why = g.at, g.why
			}
		}
		queued = append(queued, e)
	}

	// A held slot stops EVERYTHING, free-running rounds included.
	//
	// Not because they need the slot — they do not — but because Pump returns as
	// soon as it sees a slot holder, so the quota-free path that would advance
	// them is never reached while one is held. The exemption above is about the
	// account window, which genuinely does not apply to them; claiming they are
	// ready here would promise action the daemon cannot take until the holder is
	// acknowledged. (An agent's own `crq next` can still resolve such a round
	// directly, which is why this describes the queue rather than forbidding it.)
	if slotBusy {
		for i := range queued {
			queued[i].ReadyAt, queued[i].Why = time.Time{}, WaitSlotBusy
		}
		for i := range freeRunning {
			freeRunning[i].ReadyAt, freeRunning[i].Why = time.Time{}, WaitSlotBusy
		}
	}

	// One list, ordered by readiness then Seq — which is NextEligible's own rule
	// among rounds that are eligible together. Concatenating the groups instead
	// put a cooling co-only round ahead of work that could fire immediately.
	out := append(freeRunning, queued...)
	// An empty ReadyAt means two different things and they must not sort alike:
	// nothing is holding this round, or something is holding it whose end is
	// unknowable (a slot another PR holds). Rank them apart, or a slot-blocked
	// round sorts as if it were ready and can take the front.
	rank := func(e QueueEntry) int {
		switch {
		case e.ReadyAt.IsZero() && e.Why == "":
			return 0 // fire-eligible now
		case !e.ReadyAt.IsZero():
			return 1 // waiting until a known time
		default:
			return 2 // waiting on something with no knowable end
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := rank(out[i]), rank(out[j]); ri != rj {
			return ri < rj
		}
		if !out[i].ReadyAt.Equal(out[j].ReadyAt) {
			return out[i].ReadyAt.Before(out[j].ReadyAt)
		}
		return out[i].Seq < out[j].Seq
	})

	// Say which round is next ONLY when one is eligible now — then it is the
	// lowest-Seq ready round, exactly what NextEligible picks. With everything
	// still cooling, which one fires depends on when a pump happens to run: if
	// none runs between two retry times, both are eligible at the next pass and
	// the lower Seq wins, not the earlier window. So no front is claimed, and the
	// soonest opening is reported without saying whose it is.
	//
	// Only that front carries a time. Anything behind it starts when the front
	// finishes, which is the unknowable part, so naming its own gate there would
	// state a lower bound as if it were the answer.
	front := len(out) > 0 && rank(out[0]) == 0
	for i := range out {
		if front && i == 0 {
			continue // the front is the one round whose readiness is knowable
		}
		if !front && i == 0 {
			continue // nothing is eligible; the soonest opening is still worth naming
		}
		// Drop the TIME but keep the reason. A round's own gate is real and worth
		// reporting — "slot busy", "account blocked" — it just is not a start time,
		// because the round ahead has to finish first. Only a round with nothing of
		// its own holding it is purely behind.
		out[i].ReadyAt = time.Time{}
		if out[i].Why == "" {
			out[i].Why = WaitBehind
		}
	}
	return out
}

// Normalize repairs invariants after load: map init, expired retry windows
// (awaiting_retry with a passed RetryAt is simply fire-eligible; nothing to
// do), and a FireSlot no round holds and no orphaned hold keeps alive.
func (s *State) Normalize(now time.Time) {
	if s.Rounds == nil {
		s.Rounds = map[string]Round{}
	}
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	if s.FireSlot != nil && !s.SlotHeld(now) {
		s.FireSlot = nil
	}
	for key, r := range s.Rounds {
		r.foldLegacyCodex()
		r.inferCoOnly()
		s.Rounds[key] = r
	}
	for i := range s.Archive {
		s.Archive[i].foldLegacyCodex()
		s.Archive[i].inferCoOnly()
	}
	if len(s.Archive) > ArchiveMax {
		s.Archive = s.Archive[len(s.Archive)-ArchiveMax:]
	}
}

// --- round-native views consumed by crq's Wait/Loop ------------------------

// waitingHead returns the head a fired/reviewing round is currently waiting on
// (the wait IS the round), or "" when repo#pr has no active wait. Loop and Wait
// use it to tell "a review is in flight for this head" from "start a new round".
func (st *State) WaitingHead(repo string, pr int) string {
	r := st.Round(repo, pr)
	if r == nil || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) {
		return ""
	}
	return r.Head
}

// roundWaitDeadline returns the wait deadline of the fired/reviewing round at
// head, if one is set. It is the wall-clock bound Loop polls against.
func (st *State) RoundWaitDeadline(repo string, pr int, head string) (time.Time, bool) {
	r := st.Round(repo, pr)
	if r == nil || r.Head != head || (r.Phase != PhaseFired && r.Phase != PhaseReviewing) || r.WaitDeadline == nil {
		return time.Time{}, false
	}
	return r.WaitDeadline.UTC(), true
}

// containsActive reports whether repo#pr has a round still occupying its slot
// (queued through awaiting_retry) — the v2 State.Contains for the queue/inflight.
func (st *State) ContainsActive(repo string, pr int) bool {
	r := st.Round(repo, pr)
	return r != nil && r.Active()
}

// firedMarker returns the head for which repo#pr has already been requested and
// must not be re-fired without a new head — the v2 Fired[key] dedupe. A
// completed round, or one still fired/reviewing, is such a marker; a parked
// awaiting_retry round is not (Pump re-fires it once RetryAt passes).
func (st *State) FiredMarker(repo string, pr int) string {
	r := st.Round(repo, pr)
	if r == nil {
		return ""
	}
	switch r.Phase {
	case PhaseFired, PhaseReviewing, PhaseCompleted:
		return r.Head
	}
	return ""
}

// accountBlockedUntil returns the latest active block preventing repo#pr@head
// from firing: the account-wide quota block or this round's own retry window
// (the v2 feedbackBlockedUntil over Blocked + per-head Cooldown).
func (st *State) AccountBlockedUntil(repo string, pr int, head string, now time.Time) (time.Time, bool) {
	var until time.Time
	if st.Account.BlockedUntil != nil && st.Account.BlockedUntil.After(now) {
		until = st.Account.BlockedUntil.UTC()
	}
	if r := st.Round(repo, pr); r != nil && r.Phase == PhaseAwaitingRetry && r.Head == head && r.RetryAt != nil && r.RetryAt.After(now) && r.RetryAt.After(until) {
		until = r.RetryAt.UTC()
	}
	return until, !until.IsZero()
}
