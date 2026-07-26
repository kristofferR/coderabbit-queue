package main

import (
	"strings"
	"testing"
)

// The thread commands take IDs bare so a caller can clear a whole round in one
// process. The transcripts that motivated this were full of shell loops running
// one `crq resolve` subprocess per thread, every round.
func TestParseThreadCommand(t *testing.T) {
	const (
		t1 = "PRRT_kwDOAAAAAA1"
		t2 = "PRRT_kwDOAAAAAA2"
	)
	cases := []struct {
		name    string
		args    []string
		reason  bool
		want    []string
		wantRsn string
		wantRes bool
		wantErr bool
	}{
		{name: "bare ids", args: []string{t1, t2}, want: []string{t1, t2}},
		{name: "flag form still works", args: []string{"--thread", t1, "--thread", t2}, want: []string{t1, t2}},
		{name: "mixed forms", args: []string{"--thread", t1, t2}, want: []string{t1, t2}},
		{
			// The old signature demanded a target these commands never used:
			// thread node IDs are globally unique.
			name: "legacy repo and pr are dropped",
			args: []string{"owner/repo", "123", t1},
			want: []string{t1},
		},
		{
			name: "legacy target with flag threads",
			args: []string{"owner/repo", "123", "--thread", t1},
			want: []string{t1},
		},
		{
			// A repo-shaped first arg is only a target when a PR number follows,
			// so an ID that happens to contain "/" is not eaten.
			name: "slashed id without a pr number is a thread",
			args: []string{"weird/id", t1},
			want: []string{"weird/id", t1},
		},
		{name: "dangling --thread is an error", args: []string{"--thread"}, wantErr: true},
		{
			name: "unknown flag is an error, not a thread id",
			args: []string{"--treahd", t1}, wantErr: true,
		},
		{
			name: "decline collects reason and resolve",
			args: []string{t1, "--reason", "not a real issue", "--resolve"}, reason: true,
			want: []string{t1}, wantRsn: "not a real issue", wantRes: true,
		},
		{
			// resolve has no --reason, so passing one must fail rather than be
			// swallowed as a positional.
			name: "reason flag is rejected by resolve",
			args: []string{t1, "--reason", "x"}, wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			threads, reason, resolve, ok := parseThreadCommand(tc.args, tc.reason)
			if tc.wantErr {
				if ok {
					t.Fatalf("parseThreadCommand(%v) = %v, want an error", tc.args, threads)
				}
				return
			}
			if !ok {
				t.Fatalf("parseThreadCommand(%v) failed, want %v", tc.args, tc.want)
			}
			if strings.Join(threads, ",") != strings.Join(tc.want, ",") {
				t.Errorf("threads = %v, want %v", threads, tc.want)
			}
			if reason != tc.wantRsn {
				t.Errorf("reason = %q, want %q", reason, tc.wantRsn)
			}
			if resolve != tc.wantRes {
				t.Errorf("resolve = %v, want %v", resolve, tc.wantRes)
			}
		})
	}
}

// A mistyped flag used to be dropped as a non-positional, so `--wiat` ran the
// non-blocking form and looked like success.
func TestUnknownFlag(t *testing.T) {
	if _, found := unknownFlag([]string{"owner/repo", "1", "--wait"}, "--wait"); found {
		t.Error("a known flag must be accepted")
	}
	bad, found := unknownFlag([]string{"owner/repo", "1", "--wiat"}, "--wait")
	if !found || bad != "--wiat" {
		t.Errorf("unknownFlag = (%q, %v), want (--wiat, true)", bad, found)
	}
}
