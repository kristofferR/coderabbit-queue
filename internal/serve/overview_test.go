package serve

import (
	"testing"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/state"
)

func TestAutofixSessionKeepsTheHeadOwnedByItsClaim(t *testing.T) {
	now := time.Date(2026, 7, 29, 17, 0, 0, 0, time.UTC)
	key := state.Key("o/repo", 1)
	claim := state.DispatchClaim{Host: "atlas", Token: "session-1", At: now, Heartbeat: now}
	st := state.New()
	st.Rounds[key] = state.Round{Repo: "o/repo", PR: 1, Head: "new-head"}
	st.Archive = []state.Round{{
		Repo: "o/repo", PR: 1, Head: "old-head", Dispatch: &claim,
	}}
	st.Dispatches = map[string]state.DispatchClaim{key: claim}

	view := autofixView(st, now, nil)
	if len(view.Sessions) != 1 || view.Sessions[0].Head != "old-head" {
		t.Fatalf("sessions = %+v, want the live session's archived head", view.Sessions)
	}
}
