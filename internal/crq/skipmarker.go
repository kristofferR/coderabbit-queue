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
	marker := c.SkipMarker
	if marker == "" {
		return false
	}
	return strings.Contains(stripCode(body), marker)
}

// stripCode removes fenced blocks, indented blocks and inline code spans from
// Markdown. Delimiter runs are significant in fenced blocks and spans: a
// four-backtick fence may contain triple backticks, and a double-backtick span
// may contain a single literal backtick.
func stripCode(body string) string {
	return stripCodeSpans(stripIndentedCode(stripFencedCode(body)))
}

func stripFencedCode(body string) string {
	var out strings.Builder
	var fence byte
	fenceLen := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		delimiter, run, tail, ok := markdownFence(line)
		if fence == 0 {
			if ok && (delimiter != '`' || !strings.Contains(tail, "`")) {
				fence, fenceLen = delimiter, run
				if strings.HasSuffix(line, "\n") {
					out.WriteByte('\n')
				}
				continue
			}
			out.WriteString(line)
			continue
		}
		if ok && delimiter == fence && run >= fenceLen && strings.TrimSpace(tail) == "" {
			fence, fenceLen = 0, 0
		}
		if strings.HasSuffix(line, "\n") {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func stripIndentedCode(body string) string {
	var out strings.Builder
	inParagraph := false
	for _, line := range strings.SplitAfter(body, "\n") {
		if strings.TrimSpace(line) == "" {
			out.WriteString(line)
			inParagraph = false
			continue
		}
		if markdownIndent(line) >= 4 {
			// Indentation cannot interrupt a CommonMark paragraph. Without a
			// preceding blank line this is paragraph continuation, not an
			// indented code block.
			if inParagraph {
				out.WriteString(line)
				continue
			}
			if strings.HasSuffix(line, "\n") {
				out.WriteByte('\n')
			}
			continue
		}
		out.WriteString(line)
		inParagraph = continuesMarkdownParagraph(line)
	}
	return out.String()
}

func continuesMarkdownParagraph(line string) bool {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	indent := markdownIndent(line)
	if indent > 3 || indent >= len(line) {
		return false
	}
	text := line[indent:]
	if text[0] == '#' {
		end := 0
		for end < len(text) && text[end] == '#' {
			end++
		}
		if end <= 6 && (end == len(text) || text[end] == ' ' || text[end] == '\t') {
			return false
		}
	}
	return !markdownThematicBreak(text)
}

func markdownThematicBreak(text string) bool {
	var delimiter byte
	count := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case ' ', '\t':
			continue
		case '*', '-', '_':
			if delimiter == 0 {
				delimiter = text[i]
			}
			if text[i] != delimiter {
				return false
			}
			count++
		default:
			return false
		}
	}
	return count >= 3
}

func markdownIndent(line string) int {
	columns := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			columns++
		case '\t':
			columns += 4 - columns%4
		default:
			return columns
		}
	}
	return columns
}

// markdownFence recognizes a CommonMark fence delimiter: at most three leading
// spaces followed by a run of at least three backticks or tildes.
func markdownFence(line string) (delimiter byte, run int, tail string, ok bool) {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	start := 0
	for start < len(line) && start < 3 && line[start] == ' ' {
		start++
	}
	if start >= len(line) || (line[start] != '`' && line[start] != '~') {
		return 0, 0, "", false
	}
	delimiter = line[start]
	end := start
	for end < len(line) && line[end] == delimiter {
		end++
	}
	if end-start < 3 {
		return 0, 0, "", false
	}
	return delimiter, end - start, line[end:], true
}

func stripCodeSpans(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] != '`' {
			out.WriteByte(text[i])
			i++
			continue
		}
		runEnd := i
		for runEnd < len(text) && text[runEnd] == '`' {
			runEnd++
		}
		run := runEnd - i
		closeEnd := matchingBacktickRun(text, runEnd, run)
		if closeEnd < 0 {
			out.WriteString(text[i:runEnd])
			i = runEnd
			continue
		}
		// Keep prose on either side from being joined into a false marker.
		out.WriteByte(' ')
		i = closeEnd
	}
	return out.String()
}

func matchingBacktickRun(text string, start, want int) int {
	for i := start; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		end := i
		for end < len(text) && text[end] == '`' {
			end++
		}
		if end-i == want {
			return end
		}
		i = end
	}
	return -1
}
