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
		{"mentioned in a fence", "```md\n<!-- crq:skip-autoreview -->\n```\n", false},
		{"mentioned in a tilde fence", "~~~md\n<!-- crq:skip-autoreview -->\n~~~\n", false},
		{"mentioned in a blockquote fence", "> ~~~md\n> <!-- crq:skip-autoreview -->\n> ~~~\n", false},
		{"mentioned in a nested blockquote fence", "> > ```md\n> > <!-- crq:skip-autoreview -->\n> > ```\n", false},
		{"used after a blockquote fence", "> ```md\n> documented marker\n\n<!-- crq:skip-autoreview -->\n", true},
		{"mentioned in an indented block", "Example:\n\n    <!-- crq:skip-autoreview -->\n", false},
		{"mentioned in a blockquoted indented block", "> Example:\n>\n>     <!-- crq:skip-autoreview -->\n", false},
		{"mentioned in blockquoted code after prose", "Example:\n>     <!-- crq:skip-autoreview -->\n", false},
		{"used in a lazy blockquote continuation", "> Prose\n    <!-- crq:skip-autoreview -->\n", true},
		{"used after nested quote returns to a list", "> - Item\n>   > nested\n>     <!-- crq:skip-autoreview -->\n", true},
		{"mentioned in blockquoted code nested in a list", "> - Example\n>\n>       <!-- crq:skip-autoreview -->\n", false},
		{"mentioned in a tab-indented block", "Example:\n\n\t<!-- crq:skip-autoreview -->\n", false},
		{"used as indented list content", "- Review settings\n\n    <!-- crq:skip-autoreview -->\n", true},
		{"used after tab-padded list marker", "-\tOpt out\n\n    <!-- crq:skip-autoreview -->\n", true},
		{"used in a nested list item", "- Review settings\n\n  - Opt out\n\n      <!-- crq:skip-autoreview -->\n", true},
		{"mentioned in code nested in a list", "- Example\n\n      <!-- crq:skip-autoreview -->\n", false},
		{"used in a paragraph continuation", "Paragraph\n    <!-- crq:skip-autoreview -->\n", true},
		{"mentioned in code after a heading", "# Example\n    <!-- crq:skip-autoreview -->\n", false},
		{"mentioned in code after a thematic break", "***\n    <!-- crq:skip-autoreview -->\n", false},
		{"mentioned in a long fence", "````md\n```\n<!-- crq:skip-autoreview -->\n```\n````\n", false},
		{"mentioned in a long code span", "Use `` ` <!-- crq:skip-autoreview --> ` `` to document it.", false},
		{"documented and then used", "Use `<!-- crq:skip-autoreview -->`.\n\n<!-- crq:skip-autoreview -->", true},
		{"absent", "An ordinary description.", false},
		{"unclosed fence swallows the rest, as GitHub renders it", "```\n<!-- crq:skip-autoreview -->", false},
		{"shorter fence does not close a long fence", "````\n<!-- crq:skip-autoreview -->\n```\n", false},
		{"a lone backtick is literal, not a span", "a ` tick and <!-- crq:skip-autoreview -->", true},
		{"escaped backticks are literal", "Use a literal \\` here.\n\n<!-- crq:skip-autoreview -->\n\nAnd another \\` here.", true},
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

func TestSkipMarkerMatchesConfiguredWhitespaceExactly(t *testing.T) {
	cfg := Config{SkipMarker: "no review "}
	if cfg.SkipsReview("no review") {
		t.Error("a marker without its configured trailing space matched")
	}
	if !cfg.SkipsReview("no review ") {
		t.Error("the marker with its configured trailing space did not match")
	}
}

// A fenced block opens only at the start of a line. Reading a mid-line run as
// an opener left a fence that nothing could close, so everything after it was
// discarded and a marker further down went unseen — and crq fired a review the
// author had explicitly opted out of.
func TestSkipMarkerSeesPastAnInlineTripleBacktick(t *testing.T) {
	cfg := Config{SkipMarker: "<!-- crq:skip-autoreview -->"}
	for _, tc := range []struct {
		name string
		body string
		skip bool
	}{
		{
			"inline run before the marker",
			"Write ```bash``` for a shell block.\n\n<!-- crq:skip-autoreview -->\n",
			true,
		},
		{
			"a real fence still hides what it contains",
			"```\n<!-- crq:skip-autoreview -->\n```\n",
			false,
		},
		{
			"an indented fence opener still counts",
			"   ```\n<!-- crq:skip-autoreview -->\n   ```\n",
			false,
		},
		{
			"four spaces is an indented code block, not a fence",
			"    ```\n\n<!-- crq:skip-autoreview -->\n",
			true,
		},
	} {
		if got := cfg.SkipsReview(tc.body); got != tc.skip {
			t.Errorf("%s: SkipsReview = %v, want %v", tc.name, got, tc.skip)
		}
	}
}
