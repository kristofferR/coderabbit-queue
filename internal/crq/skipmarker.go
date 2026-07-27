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
	var listIndents []int
	for _, line := range strings.SplitAfter(body, "\n") {
		if strings.TrimSpace(line) == "" {
			out.WriteString(line)
			inParagraph = false
			continue
		}
		indent := markdownIndent(line)
		for len(listIndents) > 0 && indent < listIndents[len(listIndents)-1] {
			listIndents = listIndents[:len(listIndents)-1]
		}
		containerIndent := 0
		if len(listIndents) > 0 {
			containerIndent = listIndents[len(listIndents)-1]
		}
		if contentIndent, ok := markdownListContentIndent(line, containerIndent); ok {
			listIndents = append(listIndents, contentIndent)
			out.WriteString(line)
			inParagraph = continuesMarkdownParagraph(line)
			continue
		}
		relativeIndent := indent - containerIndent
		if relativeIndent >= 4 {
			// Indentation cannot interrupt a CommonMark paragraph. Without a
			// preceding blank line this is paragraph continuation, not an
			// indented code block. Indentation is relative to the active list
			// container: four absolute spaces can be ordinary list-item content.
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

// markdownListContentIndent returns the column where a list item's content
// starts. CommonMark measures an indented code block from that column, not from
// the left edge of the document.
func markdownListContentIndent(line string, containerIndent int) (int, bool) {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	column, at := leadingIndent(line)
	if column < containerIndent || column-containerIndent > 3 || at >= len(line) {
		return 0, false
	}
	start := at
	switch line[at] {
	case '-', '+', '*':
		at++
	default:
		digits := 0
		for at < len(line) && line[at] >= '0' && line[at] <= '9' && digits < 9 {
			at++
			digits++
		}
		if digits == 0 || at >= len(line) || (line[at] != '.' && line[at] != ')') {
			return 0, false
		}
		at++
	}
	markerWidth := at - start
	if at == len(line) {
		return column + markerWidth + 1, true
	}
	if line[at] != ' ' && line[at] != '\t' {
		return 0, false
	}
	padding := 0
	for at < len(line) && (line[at] == ' ' || line[at] == '\t') {
		next := padding + 1
		if line[at] == '\t' {
			currentColumn := column + markerWidth + padding
			next = padding + 4 - currentColumn%4
		}
		if next > 4 {
			break
		}
		padding = next
		at++
	}
	if padding == 0 || (at < len(line) && (line[at] == ' ' || line[at] == '\t')) {
		padding = 1
	}
	return column + markerWidth + padding, true
}

func leadingIndent(line string) (columns, bytes int) {
	for bytes < len(line) {
		switch line[bytes] {
		case ' ':
			columns++
		case '\t':
			columns += 4 - columns%4
		default:
			return columns, bytes
		}
		bytes++
	}
	return columns, bytes
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
	columns, _ := leadingIndent(line)
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
		if text[i] != '`' || backtickEscaped(text, i) {
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
		if text[i] != '`' || backtickEscaped(text, i) {
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

func backtickEscaped(text string, at int) bool {
	backslashes := 0
	for at > 0 && text[at-1] == '\\' {
		backslashes++
		at--
	}
	return backslashes%2 == 1
}
