package state

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Version tolerance: the fleet shares ONE state ref across binaries that may not
// be the same build.
//
// Go's encoding/json silently drops members it has no field for, so an older
// binary that merely reads and rewrites state erased every field a newer one had
// added. That is why Codex's per-round bookkeeping had to be dual-written into
// legacy fields, and why adding anything to a Round was a rollout problem rather
// than a change.
//
// Unknown members are therefore carried by default. A load keeps whatever it
// did not recognise, a save puts it back, and a field this binary has never
// heard of survives a foreign write untouched. A schema bump is reserved for a
// compatibility fence: newer state is refused by older binaries, as v5 requires
// so v4 pumping clients cannot ignore state-backed fleet policy.

// unknownFields holds JSON members this binary has no field for, verbatim.
type unknownFields map[string]json.RawMessage

var (
	fireSlotFields = jsonFieldNames(reflect.TypeOf(FireSlot{}))
	roundFields    = jsonFieldNames(reflect.TypeOf(Round{}))
	stateFields    = jsonFieldNames(reflect.TypeOf(State{}))
)

// UnmarshalJSON decodes a fire slot and remembers anything it did not recognise.
func (s *FireSlot) UnmarshalJSON(raw []byte) error {
	type plain FireSlot
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, fireSlotFields)
	if err != nil {
		return err
	}
	*s = FireSlot(decoded)
	s.unknown = unknown
	return nil
}

// MarshalJSON writes a fire slot back with the members it did not recognise intact.
func (s FireSlot) MarshalJSON() ([]byte, error) {
	type plain FireSlot
	return mergeUnknown(plain(s), s.unknown)
}

// UnmarshalJSON decodes a round and remembers anything it did not recognise.
func (r *Round) UnmarshalJSON(raw []byte) error {
	// A distinct type with the same layout: without it, json would call this
	// method again for the inner decode and recurse forever.
	type plain Round
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, roundFields)
	if err != nil {
		return err
	}
	*r = Round(decoded)
	r.unknown = unknown
	return nil
}

// MarshalJSON writes a round back with the members it did not recognise intact.
func (r Round) MarshalJSON() ([]byte, error) {
	type plain Round
	return mergeUnknown(plain(r), r.unknown)
}

// UnmarshalJSON decodes state and remembers anything it did not recognise.
func (s *State) UnmarshalJSON(raw []byte) error {
	type plain State
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, stateFields)
	if err != nil {
		return err
	}
	*s = State(decoded)
	s.unknown = unknown
	return nil
}

// MarshalJSON writes state back with the members it did not recognise intact.
func (s State) MarshalJSON() ([]byte, error) {
	type plain State
	return mergeUnknown(plain(s), s.unknown)
}

// captureUnknown returns the members of raw that known does not name.
func captureUnknown(raw []byte, known map[string]bool) (unknownFields, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	var out unknownFields
	for name, value := range all {
		if known[name] {
			continue
		}
		if out == nil {
			out = unknownFields{}
		}
		out[name] = value
	}
	return out, nil
}

// mergeUnknown marshals value and adds the carried members back.
//
// A carried member never shadows a field this binary owns: if a later build
// dropped a field that a foreign write still sends, what this binary computes
// now is the current truth, and the stale copy would silently win.
func mergeUnknown(value any, unknown unknownFields) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(unknown) == 0 {
		return encoded, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &members); err != nil {
		return nil, err
	}
	for name, value := range unknown {
		if _, owned := members[name]; owned {
			continue
		}
		members[name] = value
	}
	// Marshalling a map sorts its keys, so the output stays byte-stable for the
	// same content — the dashboard hash and the CAS blob both depend on that.
	return json.Marshal(members)
}

// jsonFieldNames is the set of JSON member names a struct type owns.
func jsonFieldNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue // unexported: never a JSON member
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = field.Name
		}
		names[name] = true
	}
	return names
}
