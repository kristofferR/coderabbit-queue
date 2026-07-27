package crq

import "testing"

// The pull request that DOCUMENTS the skip marker excluded itself from review,
// because the paragraph explaining the marker contains the marker. Nothing said
// so — the skip is a `continue`, so there was no round, no event and no log
// line — and it sat unreviewed for a day with feedback nobody was draining.
func TestSkipMarkerHasToBeUsedNotMentioned(t *testing.T) {
	cfg := Config{SkipMarker: "<!-- crq:skip-autoreview -->"}
	for _, tc := range []struct {
		name string
		body string
		skip bool
	}{
		{"used", "Low risk.\n\n<!-- crq:skip-autoreview -->\n", true},
		{"mentioned in a code span", "- `<!-- crq:skip-autoreview -->` stops fleet auto-review.", false},
		{"mentioned in a double-backtick code span", "- ``<!-- crq:skip-autoreview -->`` stops fleet auto-review.", false},
		{"double-backtick span may contain a backtick", "``example ` <!-- crq:skip-autoreview -->``", false},
		{"mentioned in a fence", "```md\n<!-- crq:skip-autoreview -->\n```\n", false},
		{"mentioned in a tilde fence", "~~~md\n<!-- crq:skip-autoreview -->\n~~~\n", false},
		{"backticks inside a tilde fence do not consume later prose", "~~~md\n`example`\n~~~\n<!-- crq:skip-autoreview -->", true},
		{"documented and then used", "Use `<!-- crq:skip-autoreview -->`.\n\n<!-- crq:skip-autoreview -->", true},
		{"absent", "An ordinary description.", false},
		{"unclosed fence swallows the rest, as GitHub renders it", "```\n<!-- crq:skip-autoreview -->", false},
		{"unclosed tilde fence swallows the rest", "~~~\n<!-- crq:skip-autoreview -->", false},
		{"a lone backtick is literal, not a span", "a ` tick and <!-- crq:skip-autoreview -->", true},
	} {
		if got := cfg.SkipsReview(tc.body); got != tc.skip {
			t.Errorf("%s: SkipsReview = %v, want %v", tc.name, got, tc.skip)
		}
	}

	// No marker configured means nothing is ever skipped, whatever a body says.
	if (Config{}).SkipsReview("<!-- crq:skip-autoreview -->") {
		t.Error("an empty marker skipped a pull request")
	}
}
