package crq

import (
	"context"
	"net/url"
	"testing"

	ghapi "github.com/kristofferR/coderabbit-queue/internal/gh"
)

// Inference must ask for THIS branch's pull request, not take whatever the
// repository happens to return first. A fake that ignored the head filter would
// have hidden the difference, so this pins the query itself.
func TestInferTargetAsksForTheBranchesPull(t *testing.T) {
	gh := newFakeGitHub()
	for _, tc := range []struct {
		pr     int
		branch string
	}{{11, "other-work"}, {12, "the-branch"}, {13, "unrelated"}} {
		var p ghapi.Pull
		p.State = "open"
		p.Number = tc.pr
		p.Head.SHA = "aaaaaaaaa"
		p.Head.Ref = tc.branch
		gh.pulls[fakeKey("owner/repo", tc.pr)] = p
	}

	query := url.Values{}
	query.Set("head", "owner:the-branch")
	got, err := gh.ListPulls(context.Background(), "owner/repo", query)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != 12 {
		t.Fatalf("head filter returned %+v, want only PR 12 — an unfiltered answer would let a wrong-branch lookup pass", got)
	}

	// No filter still returns everything, deterministically ordered.
	all, err := gh.ListPulls(context.Background(), "owner/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Number != 11 || all[2].Number != 13 {
		t.Fatalf("unfiltered = %+v, want 11,12,13 in order", all)
	}
}
