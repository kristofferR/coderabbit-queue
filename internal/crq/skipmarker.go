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

// stripCode removes indented and fenced blocks and inline code spans from
// Markdown.
//
// Block constructs first: a block may contain unbalanced backticks that would
// otherwise make the span pass read the rest of the document as code. Neither
// pass tries to be a Markdown parser — anything it gets wrong only means a
// marker is read where GitHub renders one, which is the reading that was already
// happening.
func stripCode(body string) string {
	var out strings.Builder
	rest := stripIndentedCode(body)
	for {
		start, fence, width := nextFence(rest)
		if start < 0 {
			break
		}
		out.WriteString(rest[:start])
		after := rest[start+width:]
		end, closingWidth := matchingFence(after, fence, width)
		if end < 0 {
			// An unclosed fence runs to the end of the body, the way GitHub
			// renders it.
			return out.String()
		}
		rest = after[end+closingWidth:]
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

func stripIndentedCode(body string) string {
	lines := strings.SplitAfter(body, "\n")
	var out strings.Builder
	blankBefore := true
	inCode := false
	for _, line := range lines {
		content := strings.TrimSuffix(line, "\n")
		content = strings.TrimSuffix(content, "\r")
		blank := strings.TrimSpace(content) == ""
		indented := strings.HasPrefix(content, "    ") || strings.HasPrefix(content, "\t")

		if inCode {
			if blank || indented {
				out.WriteByte('\n')
				continue
			}
			inCode = false
		}
		if blankBefore && indented {
			inCode = true
			out.WriteByte('\n')
			continue
		}
		out.WriteString(line)
		blankBefore = blank
	}
	return out.String()
}

func nextFence(text string) (int, byte, int) {
	backticks := strings.Index(text, "```")
	tildes := strings.Index(text, "~~~")
	at, marker := backticks, byte('`')
	if at < 0 || tildes >= 0 && tildes < at {
		at, marker = tildes, '~'
	}
	if at < 0 {
		return -1, 0, 0
	}
	return at, marker, fenceRun(text[at:], marker)
}

func matchingFence(text string, marker byte, width int) (int, int) {
	for lineStart := 0; lineStart < len(text); {
		lineEnd := strings.IndexByte(text[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(text)
		} else {
			lineEnd += lineStart
		}
		line := text[lineStart:lineEnd]
		indent := fenceIndent(line)
		if indent < len(line) && line[indent] == marker {
			closingWidth := fenceRun(line[indent:], marker)
			if closingWidth >= width && strings.TrimSpace(line[indent+closingWidth:]) == "" {
				return lineStart + indent, closingWidth
			}
		}
		if lineEnd == len(text) {
			break
		}
		lineStart = lineEnd + 1
	}
	return -1, 0
}

func fenceRun(text string, marker byte) int {
	width := 0
	for width < len(text) && text[width] == marker {
		width++
	}
	return width
}

func fenceIndent(line string) int {
	indent := 0
	for indent < len(line) && indent < 3 && line[indent] == ' ' {
		indent++
	}
	return indent
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
