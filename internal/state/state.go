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

	// RetryAt is the earliest time this head may fire again (awaiting_retry).
	RetryAt *time.Time `json:"retry_at,omitempty"`

	// LastAttemptAt is the adoption cutoff: command comments older than the
	// most recent failed/abandoned attempt must not be adopted as this round's
	// fire.
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`

	// WaitDeadline bounds how long a fired/reviewing round is waited on before
	// the round is retried or surfaced as timed out.
	WaitDeadline *time.Time `json:"wait_deadline,omitempty"`

	Token  string `json:"token,omitempty"` // reservation token (CAS race detection)
	ByHost string `json:"by_host,omitempty"`
	Note   string `json:"note,omitempty"` // human-readable reason for the last transition
}

// FireSlot is the single global in-flight reservation: at most one review
// command may be getting posted at a time, fleet-wide.
type FireSlot struct {
	Key   string    `json:"key"` // repo#pr holding the slot
	Token string    `json:"token"`
	Since time.Time `json:"since"`
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

	// Archive keeps recently finished rounds (superseded, closed, cancelled)
	// for the dashboard and debugging. Bounded by ArchiveMax.
	Archive []Round `json:"archive,omitempty"`

	Warn         string     `json:"warn,omitempty"`
	UpdatedAt    *time.Time `json:"wrote_at,omitempty"`
	DashboardSHA string     `json:"dashboard_sha,omitempty"`
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
// awaiting_retry) → reserved.
func (r *Round) Reserve(token, host string, now time.Time) error {
	if r.Phase != PhaseQueued && !r.retryEligible(now) {
		return r.illegal(PhaseReserved)
	}
	r.Phase = PhaseReserved
	r.Token = token
	r.ByHost = host
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

// Normalize repairs invariants after load: map init, expired retry windows
// (awaiting_retry with a passed RetryAt is simply fire-eligible; nothing to
// do), and a FireSlot pointing at a round that no longer holds it.
func (s *State) Normalize(now time.Time) {
	if s.Rounds == nil {
		s.Rounds = map[string]Round{}
	}
	if s.Version == 0 {
		s.Version = SchemaVersion
	}
	if s.FireSlot != nil && s.SlotRound() == nil {
		s.FireSlot = nil
	}
	if len(s.Archive) > ArchiveMax {
		s.Archive = s.Archive[len(s.Archive)-ArchiveMax:]
	}
}
