package crq

import "strings"

// SkipsReview reports whether a pull request body asks the fleet not to review
// it.
//
// The marker has to be USED, not merely mentioned. A pull request that
// documents the marker — inside backticks, in the paragraph explaining what it
// does — was silently excluded from review and from fix sessions by the very
// text describing the feature. It went unreviewed for a day and nothing said
// why: the skip is a `continue`, so there is no round, no event and no log line
// to find.
//
// So code spans and fenced blocks are removed before looking. That is where a
// mention lives and an instruction does not: an instruction is an HTML comment
// in the prose, which is exactly what survives this.
func (c Config) SkipsReview(body string) bool {
	marker := strings.TrimSpace(c.SkipMarker)
	if marker == "" {
		return false
	}
	return strings.Contains(stripCode(body), marker)
}

// stripCode removes fenced blocks and inline code spans from Markdown.
//
// Fences first: a fence may contain unbalanced backticks that would otherwise
// make the span pass read the rest of the document as code. Neither pass tries
// to be a Markdown parser — anything it gets wrong only means a marker is read
// where GitHub renders one, which is the reading that was already happening.
func stripCode(body string) string {
	var out strings.Builder
	rest := body
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			break
		}
		out.WriteString(rest[:start])
		after := rest[start+3:]
		end := strings.Index(after, "```")
		if end < 0 {
			// An unclosed fence runs to the end of the body, the way GitHub
			// renders it.
			return out.String()
		}
		rest = after[end+3:]
	}
	out.WriteString(rest)

	text := out.String()
	var spanned strings.Builder
	for len(text) > 0 {
		start := strings.IndexByte(text, '`')
		if start < 0 {
			break
		}
		spanned.WriteString(text[:start])
		run := backtickRun(text[start:])
		after := text[start+run:]
		end := matchingBacktickRun(after, run)
		if end < 0 {
			// An unmatched delimiter is literal, not the start of a span.
			spanned.WriteString(text[start:])
			return spanned.String()
		}
		text = after[end+run:]
	}
	spanned.WriteString(text)
	return spanned.String()
}

func backtickRun(text string) int {
	n := 0
	for n < len(text) && text[n] == '`' {
		n++
	}
	return n
}

func matchingBacktickRun(text string, want int) int {
	for at := 0; at < len(text); {
		next := strings.IndexByte(text[at:], '`')
		if next < 0 {
			return -1
		}
		at += next
		if run := backtickRun(text[at:]); run == want {
			return at
		} else {
			at += run
		}
	}
	return -1
}
