package state

import "time"

// FleetDefaults is what every repository inherits, recorded once for the whole
// fleet instead of in each host's env file.
//
// It exists for the same reason the enrollment record does: the settings that
// govern a fleet lived in ~/.config/crq/env on whichever machine happened to
// run the daemon, so changing one meant editing a file per host and hoping they
// agreed. A record here is one answer every host reads.
//
// Every field is optional and absent means "keep using this host's env value",
// so a fleet that never writes one behaves exactly as before. That is also what
// makes the record safe to add to: a field a newer binary sets is preserved by
// an older one through the tolerant round-trip, and simply not acted on.
type FleetDefaults struct {
	// CoBots/Required are the fleet's reviewer defaults, the base a per-repo
	// override starts from. Set* distinguishes "not chosen" from "chosen to be
	// none", which a JSON round trip would otherwise flatten.
	CoBots      []string `json:"cobots,omitempty"`
	Required    []string `json:"required,omitempty"`
	SetCoBots   bool     `json:"set_cobots,omitempty"`
	SetRequired bool     `json:"set_required,omitempty"`

	// MinInterval is the pacing floor between metered fires, as a duration
	// string ("90s"). A string rather than a number because the unit is part of
	// the value, and a bare integer in JSON invites reading it as the wrong one.
	MinInterval string `json:"min_interval,omitempty"`

	// WeeklyLimit is the vendor's weekly fair-use threshold. A pointer so 0 can
	// mean "count, but forecast nothing" rather than "unset".
	WeeklyLimit *int `json:"weekly_limit,omitempty"`

	// AutofixDefault is whether a repository with no explicit switch may be
	// fixed. Absent means yes, which is what crq has always done.
	AutofixDefault *bool `json:"autofix_default,omitempty"`

	// Solver is the fleet's fix-session default, which a repository's own record
	// is layered over.
	Solver SolverSettings `json:"solver,omitempty"`

	By        string     `json:"by,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Empty reports whether the record says nothing at all. A record that has been
// emptied field by field must not keep reading as "recorded": every setting
// would show its env value while the fleet claimed to have an answer.
func (f FleetDefaults) Empty() bool {
	return !f.SetCoBots && !f.SetRequired && f.MinInterval == "" &&
		f.WeeklyLimit == nil && f.AutofixDefault == nil && f.Solver.Empty()
}

// AutofixDefaultOn is the answer for a repository with no explicit switch.
func (s *State) AutofixDefaultOn() bool {
	if s.Fleet.AutofixDefault == nil {
		return true
	}
	return *s.Fleet.AutofixDefault
}

// SetFleetDefaults records the fleet defaults, stamping who and when.
func (s *State) SetFleetDefaults(fd FleetDefaults, by string, now time.Time) {
	if fd.Empty() {
		s.Fleet = FleetDefaults{}
		return
	}
	at := now.UTC()
	fd.By, fd.UpdatedAt = by, &at
	s.Fleet = fd
}
