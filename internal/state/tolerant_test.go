package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The fleet shares one state ref across binaries that may not be the same build,
// and encoding/json drops members it has no field for — so an older binary that
// merely read and rewrote state erased whatever a newer one had added. This is the
// property that makes adding a field safe: what this binary does not understand,
// it carries.
func TestUnknownRoundFieldsSurviveARewrite(t *testing.T) {
	// State as a NEWER binary would write it: a round carrying a field this build
	// has never heard of, and a top-level member likewise.
	foreign := `{
	  "v": 3, "rev": 7, "next_seq": 9,
	  "rounds": {
	    "owner/repo#1": {
	      "repo": "owner/repo", "pr": 1, "head": "abcdef123", "seq": 1,
	      "phase": "queued", "enqueued_at": "2026-07-26T12:00:00Z",
	      "dispatch": {"claimed_at": "2026-07-26T12:01:00Z", "attempts": 2},
	      "some_future_flag": true
	    }
	  },
	  "account": {"scope": "owner"},
	  "future_top_level": {"nested": ["a", "b"]}
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	// The known fields still decode normally.
	round, ok := st.Rounds["owner/repo#1"]
	if !ok || round.PR != 1 || round.Phase != PhaseQueued {
		t.Fatalf("known fields must decode: %+v", round)
	}

	// Now rewrite it, exactly as a CAS would.
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["future_top_level"]; !ok {
		t.Errorf("a top-level member this binary does not know was dropped:\n%s", out)
	}
	rounds, _ := back["rounds"].(map[string]any)
	written, _ := rounds["owner/repo#1"].(map[string]any)
	if written == nil {
		t.Fatalf("the round vanished:\n%s", out)
	}
	for _, key := range []string{"dispatch", "some_future_flag"} {
		if _, ok := written[key]; !ok {
			t.Errorf("round member %q was dropped — this is the erasure the carrier exists to prevent:\n%s", key, out)
		}
	}
	// And the carried members keep their shape, not just their names.
	dispatch, _ := written["dispatch"].(map[string]any)
	if dispatch == nil || dispatch["attempts"] != float64(2) {
		t.Errorf("carried member lost its content: %#v", written["dispatch"])
	}
}

// FireSlot is nested beneath a known State field, so top-level tolerance cannot
// carry additions made to the slot itself.
func TestUnknownFireSlotFieldsSurviveARewrite(t *testing.T) {
	foreign := `{
	  "v": 3, "rev": 7, "next_seq": 9,
	  "rounds": {
	    "owner/repo#1": {
	      "repo": "owner/repo", "pr": 1, "head": "abcdef123", "seq": 1,
	      "phase": "reserved", "enqueued_at": "2026-07-26T12:00:00Z",
	      "token": "slot-token"
	    }
	  },
	  "fire_slot": {
	    "key": "owner/repo#1", "token": "slot-token",
	    "since": "2026-07-26T12:01:00Z",
	    "future_hold": {"until": "2026-07-26T12:05:00Z"}
	  },
	  "account": {"scope": "owner"}
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	slot, _ := back["fire_slot"].(map[string]any)
	if slot == nil {
		t.Fatalf("the fire slot vanished:\n%s", out)
	}
	hold, _ := slot["future_hold"].(map[string]any)
	if hold == nil || hold["until"] != "2026-07-26T12:05:00Z" {
		t.Errorf("carried fire-slot member lost its content: %#v", slot["future_hold"])
	}
}

// A carried member must never win over a field this binary owns: what this build
// computes now is the current truth, and a stale copy from a foreign write would
// silently override it.
func TestCarriedMembersNeverShadowOwnedFields(t *testing.T) {
	// "note" is a field this binary owns; pretend a foreign payload also sent it
	// (it would normally decode, so force the carrier directly).
	round := Round{Repo: "owner/repo", PR: 1, Head: "abcdef123", Note: "current"}
	round.unknown = unknownFields{"note": json.RawMessage(`"stale"`)}

	out, err := json.Marshal(round)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back["note"] != "current" {
		t.Errorf("note = %v, want the value this binary computed", back["note"])
	}
}

// The CAS blob and the dashboard hash both depend on the same content producing
// the same bytes, so carrying members must not make the output order vary.
func TestRewriteIsByteStable(t *testing.T) {
	foreign := `{"v":3,"rev":1,"next_seq":1,"rounds":{},"account":{},"zzz":1,"aaa":2,"mmm":3}`
	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("output is not stable across marshals:\n%s\n%s", first, again)
		}
	}
}

// Round-tripping must not disturb the invariants Normalize repairs, nor the
// legacy Codex folding that shares the same struct.
func TestRewritePreservesNormalizeAndLegacyFolding(t *testing.T) {
	now := time.Now().UTC()
	commanded := now.Add(-time.Minute)
	foreign := `{
	  "v": 3, "rev": 1, "next_seq": 2,
	  "rounds": {
	    "owner/repo#1": {
	      "repo": "owner/repo", "pr": 1, "head": "abcdef123", "seq": 1,
	      "phase": "fired", "enqueued_at": "` + now.Format(time.RFC3339) + `",
	      "codex_command_id": 4242,
	      "codex_commanded_at": "` + commanded.Format(time.RFC3339) + `",
	      "unheard_of": "keep me"
	    }
	  },
	  "account": {}
	}`
	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	st.Normalize(now)

	round := st.Rounds["owner/repo#1"]
	if co := round.Co(codexCoBotKey); co.CommandID != 4242 {
		t.Errorf("legacy Codex fields must still fold into CoBots, got %+v", co)
	}
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "unheard_of") {
		t.Errorf("the carried member did not survive Normalize:\n%s", out)
	}
	if !strings.Contains(string(out), "codex_command_id") {
		t.Errorf("the legacy dual-write must still be emitted:\n%s", out)
	}
}
