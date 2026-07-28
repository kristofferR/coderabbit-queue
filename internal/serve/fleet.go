package serve

import (
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
	"github.com/kristofferR/coderabbit-queue/internal/state"
)

// FleetConfig is the configuration the dashboard displays. It is a copy rather
// than a reference to crq.Config so this package never imports the orchestrator
// (which would make the dependency graph a cycle) and so what the UI can see is
// an explicit, reviewable list.
type FleetConfig struct {
	GateRepo       string   `json:"gate_repo"`
	StateRef       string   `json:"state_ref"`
	DashboardIssue int      `json:"dashboard_issue,omitempty"`
	CalibrationPR  int      `json:"calibration_pr,omitempty"`
	Scope          []string `json:"scope,omitempty"`
	AllowRepos     []string `json:"allow_repos,omitempty"`
	ExcludeRepos   []string `json:"exclude_repos,omitempty"`
	SkipAuthors    []string `json:"skip_authors,omitempty"`
	SkipMarker     string   `json:"skip_marker,omitempty"`

	MinInterval     Dur `json:"min_interval"`
	InflightTimeout Dur `json:"inflight_timeout"`
	WatchInterval   Dur `json:"watch_interval"`

	Reviewers []ReviewerCfg `json:"reviewers"`

	AutofixCommand     []string `json:"autofix_command,omitempty"`
	AutofixMaxAttempts int      `json:"autofix_max_attempts,omitempty"`
	AutofixConcurrency int      `json:"autofix_concurrency,omitempty"`
	AutofixForks       bool     `json:"autofix_forks,omitempty"`
	WorkspaceRoot      string   `json:"workspace_root,omitempty"`
}

// Dur renders as the duration string a person would type into the env file,
// not as nanoseconds.
type Dur time.Duration

func (d Dur) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

type ReviewerCfg struct {
	Login    string `json:"login"`
	Name     string `json:"name"`
	Primary  bool   `json:"primary"`
	Required bool   `json:"required"`
	Metered  bool   `json:"metered"`
	Command  string `json:"command,omitempty"`
	Trigger  string `json:"trigger,omitempty"`
	Grace    Dur    `json:"grace,omitempty"`
}

// Snapshot is everything the dashboard reads, reduced once per state change.
// One payload keeps every page consistent: two endpoints could otherwise
// disagree about the same revision.
type Snapshot struct {
	Overview Overview     `json:"overview"`
	Repos    []RepoRow    `json:"repos"`
	Bots     []BotCard    `json:"bots"`
	Setup    SetupView    `json:"setup"`
	Settings SettingsView `json:"settings"`
	// Events is live-only, since this server started. See events.go.
	Events []Event `json:"events"`
}

// RepoRow is one repository as the Repos page lists it.
type RepoRow struct {
	Repo string `json:"repo"`
	// Enrollment is where the decision comes from, not merely whether it is on:
	// a repo forced in by a host's env file cannot be turned off from here, and
	// saying "managed" would invite someone to try.
	Enrollment string `json:"enrollment"` // state|env|excluded|scope|off
	EnvHost    string `json:"env_host,omitempty"`
	// Reviewed is the resolved answer; Enrollment is only where it came from.
	Reviewed     bool       `json:"reviewed"`
	EnvConflict  bool       `json:"env_conflict,omitempty"`
	EnrollReason string     `json:"enroll_reason,omitempty"`
	EnrollBy     string     `json:"enroll_by,omitempty"`
	EnrollAt     *time.Time `json:"enroll_at,omitempty"`

	// Reviewers/Required are the RESOLVED sets — what will actually run here,
	// not the raw override. PrimaryOff is called out separately because it is
	// the one absence a reader would otherwise misread as a fleet without a
	// metered reviewer at all.
	Reviewers  []string   `json:"reviewers"`
	Required   []string   `json:"required"`
	PrimaryOff bool       `json:"primary_off,omitempty"`
	Override   bool       `json:"override"`
	OverrideBy string     `json:"override_by,omitempty"`
	OverrideAt *time.Time `json:"override_at,omitempty"`

	Autofix       string     `json:"autofix"` // default|on|off
	AutofixReason string     `json:"autofix_reason,omitempty"`
	AutofixBy     string     `json:"autofix_by,omitempty"`
	AutofixAt     *time.Time `json:"autofix_at,omitempty"`

	ActiveRounds int `json:"active_rounds"`
	QueuedRounds int `json:"queued_rounds"`
	HeldPRs      int `json:"held_prs"`
	Fixing       int `json:"fixing"`
}

// BotCard is one reviewer on the Bots page. "Last seen" is deliberately what
// crq itself recorded — a trigger it posted or a claim it observed — rather
// than a vendor status we would have to guess at.
type BotCard struct {
	Login     string     `json:"login"`
	Name      string     `json:"name"`
	Primary   bool       `json:"primary"`
	Metered   bool       `json:"metered"`
	Enabled   bool       `json:"enabled"`
	Required  bool       `json:"required"`
	Command   string     `json:"command,omitempty"`
	Trigger   string     `json:"trigger,omitempty"`
	Grace     Dur        `json:"grace,omitempty"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	SeenOn    string     `json:"seen_on,omitempty"`
	RepoCount int        `json:"repo_count"`
}

type SetupView struct {
	Checks []Check    `json:"checks"`
	Tools  []Tool     `json:"tools"`
	Hosts  []HostInfo `json:"hosts"`
	// ToolsHost names the machine the tool list describes. crq stores no tool
	// inventory for other hosts, so claiming a fleet-wide view would be a lie.
	ToolsHost string `json:"tools_host"`
}

type Check struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"` // ok|warn|bad|unknown
	Detail string `json:"detail,omitempty"`
}

type Tool struct {
	Name     string `json:"name"`
	Purpose  string `json:"purpose"`
	Required bool   `json:"required"`
	Found    bool   `json:"found"`
	Path     string `json:"path,omitempty"`
}

type HostInfo struct {
	Name      string     `json:"name"`
	Roles     []string   `json:"roles,omitempty"`
	Health    string     `json:"health,omitempty"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	Failures  int        `json:"failures,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	// Caps is the capability bitmask the writer reported. A host on an older
	// binary reports fewer bits and silently ignores settings it cannot honor.
	Caps int `json:"caps,omitempty"`
}

type SettingsView struct {
	Config   FleetConfig `json:"config"`
	Quota    Quota       `json:"quota"`
	Plumbing []KV        `json:"plumbing"`
}

type KV struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Detail string `json:"detail,omitempty"`
}

// BuildFleet reduces everything the non-overview pages read.
func BuildFleet(st state.State, cfg FleetConfig, ov Overview, tools []Tool, toolsHost string, now time.Time, botsFor BotsFor, enrollFor EnrollFor) Snapshot {
	return Snapshot{
		Overview: ov,
		Repos:    repoRows(st, cfg, now, botsFor, enrollFor),
		Bots:     botCards(st, cfg),
		Setup:    setupView(st, cfg, ov, tools, toolsHost),
		Settings: SettingsView{Config: cfg, Quota: ov.Quota, Plumbing: plumbing(st, cfg)},
	}
}

func repoRows(st state.State, cfg FleetConfig, now time.Time, botsFor BotsFor, enrollFor EnrollFor) []RepoRow {
	rows := map[string]*RepoRow{}
	get := func(repo string) *RepoRow {
		key := strings.ToLower(repo)
		if r, ok := rows[key]; ok {
			return r
		}
		r := &RepoRow{Repo: repo, Enrollment: "off", Autofix: "default"}
		rows[key] = r
		return r
	}

	// Every source that can mention a repo contributes a row. A repo that only
	// appears as a hold still belongs in the list: a hold is an operator
	// decision, and dropping it would hide one.
	for _, r := range st.Rounds {
		row := get(r.Repo)
		switch r.Phase {
		case state.PhaseQueued, state.PhaseAwaitingRetry:
			row.QueuedRounds++
			row.ActiveRounds++
		case state.PhaseReserved, state.PhaseFired, state.PhaseReviewing:
			row.ActiveRounds++
		}
	}
	for key := range st.Holds {
		repo, _ := splitKey(key)
		get(repo).HeldPRs++
	}
	for key := range st.Dispatches {
		repo, _ := splitKey(key)
		get(repo).Fixing++
	}
	for repo, rv := range st.Repos {
		row := get(repo)
		row.Override = rv.SetCoBots || rv.SetRequired || rv.PrimaryOff
		row.OverrideBy, row.OverrideAt = rv.By, rv.UpdatedAt
		row.PrimaryOff = rv.PrimaryOff
	}
	for repo, sw := range st.RepoAutofix {
		row := get(repo)
		if sw.Enabled {
			row.Autofix = "on"
		} else {
			row.Autofix = "off"
		}
		row.AutofixReason, row.AutofixBy, row.AutofixAt = sw.Reason, sw.By, sw.UpdatedAt
	}
	for repo := range cfg.allowSet() {
		get(repo)
	}
	// A repository turned off from here has no rounds, no holds and no env
	// mention, so nothing above would list it — and an "off" nobody can see is
	// how a project quietly stops being reviewed.
	for _, repo := range st.EnrolledRepos() {
		get(repo)
	}

	// Resolved, not merged here: an override names co-reviewers by login while
	// the fleet default names them by short name, and half a repository's
	// answer (say, only its required set) still inherits the other half. Asking
	// the resolver for every row is the only way the list means one thing.
	for _, row := range rows {
		row.Reviewers, row.Required = nil, nil
		for _, b := range botsFor(row.Repo) {
			row.Reviewers = append(row.Reviewers, b.Name)
			if b.Required {
				row.Required = append(row.Required, b.Name)
			}
		}
		if enrollFor != nil {
			e := enrollFor(st, row.Repo)
			row.Enrollment, row.Reviewed, row.EnvConflict = e.Source, e.Enabled, e.EnvConflict
			row.EnrollReason, row.EnrollBy, row.EnrollAt = e.Reason, e.By, e.UpdatedAt
		} else {
			row.Enrollment = cfg.enrollmentOf(row.Repo)
			row.Reviewed = row.Enrollment == "env" || row.Enrollment == "scope"
		}
	}

	out := make([]RepoRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out
}

func (c FleetConfig) allowSet() map[string]bool {
	set := map[string]bool{}
	for _, r := range c.AllowRepos {
		set[r] = true
	}
	return set
}

func (c FleetConfig) enrollmentOf(repo string) string {
	lower := strings.ToLower(repo)
	for _, r := range c.ExcludeRepos {
		if strings.ToLower(r) == lower {
			return "excluded"
		}
	}
	for _, r := range c.AllowRepos {
		if strings.ToLower(r) == lower {
			return "env"
		}
	}
	if len(c.AllowRepos) == 0 {
		// With no allowlist the whole scope is in play, so a repo crq has seen
		// is being reviewed by virtue of its owner.
		return "scope"
	}
	return "unknown"
}

func (c FleetConfig) fleetReviewers() (bots []string, required []string) {
	for _, r := range c.Reviewers {
		bots = append(bots, r.Name)
		if r.Required {
			required = append(required, r.Name)
		}
	}
	return bots, required
}

func botCards(st state.State, cfg FleetConfig) []BotCard {
	seen := map[string]*time.Time{}
	where := map[string]string{}
	note := func(login string, at *time.Time, repo string, pr int) {
		if at == nil {
			return
		}
		key := dialect.NormalizeBotName(login)
		if cur, ok := seen[key]; !ok || cur == nil || at.After(*cur) {
			seen[key] = at
			where[key] = state.Key(repo, pr)
		}
	}
	scan := func(r state.Round) {
		for login, co := range r.CoBots {
			if co.CommandedAt != nil {
				note(login, co.CommandedAt, r.Repo, r.PR)
			} else if co.ClaimedAt != nil {
				note(login, co.ClaimedAt, r.Repo, r.PR)
			}
		}
		if r.FiredAt != nil && !r.CoOnly {
			note(cfg.primaryLogin(), r.FiredAt, r.Repo, r.PR)
		}
	}
	for _, r := range st.Rounds {
		scan(r)
	}
	for _, r := range st.Archive {
		scan(r)
	}

	repoCount := map[string]int{}
	for _, rv := range st.Repos {
		for _, b := range rv.CoBots {
			repoCount[dialect.NormalizeBotName(b)]++
		}
	}

	out := make([]BotCard, 0, len(cfg.Reviewers))
	for _, r := range cfg.Reviewers {
		key := dialect.NormalizeBotName(r.Login)
		card := BotCard{
			Login: r.Login, Name: r.Name, Primary: r.Primary, Metered: r.Metered,
			Enabled: true, Required: r.Required, Command: r.Command,
			Trigger: r.Trigger, Grace: r.Grace,
			LastSeen: seen[key], SeenOn: where[key], RepoCount: repoCount[key],
		}
		out = append(out, card)
	}
	return out
}

func (c FleetConfig) primaryLogin() string {
	for _, r := range c.Reviewers {
		if r.Primary {
			return r.Login
		}
	}
	return c.GateRepo // never matches a bot; keeps note() harmless
}

func setupView(st state.State, cfg FleetConfig, ov Overview, tools []Tool, toolsHost string) SetupView {
	v := SetupView{Tools: tools, ToolsHost: toolsHost, Checks: []Check{}, Hosts: []HostInfo{}}

	add := func(key, label, status, detail string) {
		v.Checks = append(v.Checks, Check{Key: key, Label: label, Status: status, Detail: detail})
	}
	add("state", "Queue home", "ok",
		cfg.GateRepo+" · ref "+cfg.StateRef+" · rev "+itoa(ov.Rev))
	if cfg.DashboardIssue > 0 {
		add("dashboard", "Markdown dashboard", "ok", "issue #"+itoa(int64(cfg.DashboardIssue)))
	} else {
		add("dashboard", "Markdown dashboard", "warn", "not configured (CRQ_ISSUE)")
	}
	if cfg.CalibrationPR > 0 {
		add("calibration", "Quota calibration", "ok", "PR #"+itoa(int64(cfg.CalibrationPR)))
	} else {
		add("calibration", "Quota calibration", "warn", "no calibration PR — quota is guessed from notices")
	}
	switch {
	case ov.Leader == nil:
		add("leader", "Review daemon", "bad", "no host holds the leader lease")
	case ov.Leader.Expired:
		add("leader", "Review daemon", "warn", "lease expired · last held by "+ov.Leader.Host)
	default:
		add("leader", "Review daemon", "ok", "leader "+ov.Leader.Host)
	}
	missing := 0
	for _, t := range tools {
		if t.Required && !t.Found {
			missing++
		}
	}
	if missing == 0 {
		add("tools", "Required tools", "ok", "present on "+toolsHost)
	} else {
		add("tools", "Required tools", "bad", itoa(int64(missing))+" missing on "+toolsHost)
	}
	if len(ov.Autofix.Hosts) == 0 {
		add("autofix", "Autofix", "unknown", "no host has reported a fix session")
	} else {
		bad := 0
		for _, h := range ov.Autofix.Hosts {
			if h.Health == "unhealthy" {
				bad++
			}
		}
		if bad > 0 {
			add("autofix", "Autofix", "bad", itoa(int64(bad))+" host(s) failing")
		} else {
			add("autofix", "Autofix", "ok", itoa(int64(len(ov.Autofix.Hosts)))+" host(s) reporting")
		}
	}

	// Hosts merge three sources: who writes state, who runs autofix, and who
	// holds the lease.
	hosts := map[string]*HostInfo{}
	get := func(name string) *HostInfo {
		if h, ok := hosts[name]; ok {
			return h
		}
		h := &HostInfo{Name: name}
		hosts[name] = h
		return h
	}
	for id, w := range st.Writers {
		h := get(hostOf(id))
		at := w.At
		h.LastSeen = &at
		h.Caps = w.Caps
	}
	for _, ah := range ov.Autofix.Hosts {
		h := get(ah.Name)
		h.Health, h.Failures, h.LastError = ah.Health, ah.Failures, ah.LastError
		h.Roles = append(h.Roles, "autofix")
	}
	if ov.Leader != nil && !ov.Leader.Expired {
		get(ov.Leader.Host).Roles = append(get(ov.Leader.Host).Roles, "leader")
	}
	for _, h := range hosts {
		sort.Strings(h.Roles)
		v.Hosts = append(v.Hosts, *h)
	}
	sort.Slice(v.Hosts, func(i, j int) bool { return v.Hosts[i].Name < v.Hosts[j].Name })
	return v
}

func plumbing(st state.State, cfg FleetConfig) []KV {
	out := []KV{
		{Key: "Gate repo", Value: cfg.GateRepo, Detail: "holds the state ref, dashboard issue and calibration PR"},
		{Key: "State ref", Value: cfg.StateRef, Detail: "schema v" + itoa(int64(st.Version))},
		{Key: "Revision", Value: itoa(st.Rev)},
	}
	if st.UpdatedAt != nil {
		out = append(out, KV{Key: "Last written", Value: st.UpdatedAt.Format(time.RFC3339)})
	}
	if st.Leader != nil {
		out = append(out, KV{Key: "Leader lease", Value: hostOf(st.Leader.Owner),
			Detail: "expires " + st.Leader.ExpiresAt.Format(time.RFC3339)})
	}
	if cfg.DashboardIssue > 0 {
		out = append(out, KV{Key: "Markdown dashboard", Value: "issue #" + itoa(int64(cfg.DashboardIssue)),
			Detail: "still updated — the web dashboard is additive"})
	}
	if cfg.CalibrationPR > 0 {
		out = append(out, KV{Key: "Calibration PR", Value: "#" + itoa(int64(cfg.CalibrationPR))})
	}
	out = append(out, KV{Key: "Writers seen", Value: itoa(int64(len(st.Writers))), Detail: "24h window"})
	return out
}

// LocalTools probes this machine only. crq keeps no tool inventory for other
// hosts, and inventing one from a version string would be worse than saying so.
func LocalTools() []Tool {
	want := []struct {
		name, purpose string
		required      bool
	}{
		{"crq", "the binary itself — every host must run the same version", true},
		{"git", "repository mirrors and worktrees for fix sessions", true},
		{"gh", "GitHub CLI — where the token comes from", true},
		{"claude", "default autofix agent", false},
		{"codex", "alternative autofix agent", false},
		{"coderabbit", "local preflight review before pushing", false},
		{"macroscope", "second local opinion before pushing", false},
	}
	out := make([]Tool, 0, len(want))
	for _, w := range want {
		t := Tool{Name: w.name, Purpose: w.purpose, Required: w.required}
		if path, err := exec.LookPath(w.name); err == nil {
			t.Found, t.Path = true, path
		}
		out = append(out, t)
	}
	return out
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
