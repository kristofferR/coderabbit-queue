package dialect

import (
	"regexp"
	"strings"
)

// markup matches the wrappers a bot puts AROUND its title: badge images, the
// specific HTML tags the bots use, and emphasis runs that delimit a span.
//
// Both halves are deliberately narrow. A generic <[^>]+> would eat `Map<string,
// User>` from a title, and stripping every # would turn "#123" into "123", so
// only known tags and boundary emphasis go. Heading markers are handled by
// headingOf, which is where they mean something.
var markup = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)` +
	`|</?(?i:sub|sup|details|summary|b|i|em|strong|br|img|div|span|p|a|code)(?:\s[^>]*)?/?>` +
	"|(^|\\s)[*_`]+|[*_`]+(\\s|$)")

// rubric matches CodeRabbit's fixed severity header — "🎯 Functional
// Correctness | 🟡 Minor | ⚡ Quick win" — which is the same on every finding
// and displaces the part that differs. It matches the SHAPE rather than any
// pipe, because a title may contain one: "Support A | B configuration" must not
// become "B configuration".
var rubric = regexp.MustCompile(`^[^|]*\b(Correctness|Maintainability|Security|Performance|Reliability|Quality)\b[^|]*\|[^|]*\|[^|]*\|?\s*`)

// severityWord is the vocabulary Bugbot and Macroscope lead with.
const severityWord = `critical|high|medium|low|major|minor|blocker|trivial|info`

// severityOnly matches a title that is nothing but a severity label — "High
// Severity", "Critical" — which carries nothing a severity field does not.
var severityOnly = regexp.MustCompile(`(?i)^\W*(` + severityWord + `)(\s+severity)?\W*$`)

// severityPrefix matches a leading severity label, which Macroscope puts in
// front of the path on its first line.
var severityPrefix = regexp.MustCompile(`(?i)^\W*(` + severityWord + `)(\s+severity)?\b[\s:—-]*`)

// ThreadTitle reduces a review comment to one readable line, per bot.
//
// Every bot wraps its summary differently: CodeRabbit prefixes a fixed severity
// rubric, Codex leads with a badge image, Bugbot opens with a `###` heading
// followed by a bold severity, and Macroscope starts with an emoji severity and
// the path. Taking the first bold span works for two of them and reports "High
// Severity" for the others. It returns "" when nothing in the comment describes
// the finding, since a non-descriptive title is worse than none.
//
// It lives here because this is where bot wording lives; a listing in the
// orchestration layer must not have to know how a bot writes a heading. Whether
// the author IS a reviewer is the caller's question, not this package's: it
// depends on the configured primary and on matching logins rather than config
// names, and a person whose handle happens to be "codex" is not the bot.
func ThreadTitle(bot bool, body string) string {
	var candidates []string
	if bot {
		// A heading first, for the bots that lead with a severity label and put
		// the summary in one; then the bold span, then the first prose line,
		// which is where Macroscope's summary lives.
		candidates = append(candidates, headingOf(body), TitleFromDetailedBlock(body), firstProse(body))
	} else {
		// A person's first line is what they meant to say. Preferring a bold
		// span picks up a label they wrote later — "Suggestion:" instead of the
		// question they asked.
		candidates = append(candidates, firstProse(body), TitleOf(body))
	}

	for _, candidate := range candidates {
		if title := usableTitle(candidate); title != "" {
			return title
		}
	}
	// Everything reduced to a severity label or a path. Empty is the honest
	// answer: the listing omits the title and still shows path, line and URL,
	// which is strictly more than "High Severity" would have said.
	return ""
}

// usableTitle is a cleaned candidate that actually says something: not a bare
// severity label, and not just the file it points at.
func usableTitle(candidate string) string {
	title := cleanTitle(candidate)
	if title == "" || severityOnly.MatchString(title) {
		return ""
	}
	// A line that is only the rubric describes every finding equally, so it
	// describes none of them.
	if rubric.MatchString(candidate) && strings.TrimSpace(rubric.ReplaceAllString(cleanTitle(candidate), "")) == "" {
		return ""
	}
	if rest := strings.TrimSpace(severityPrefix.ReplaceAllString(title, "")); rest == "" || pathLike(rest) {
		return ""
	}
	return title
}

// pathLike reports whether text is a bare file reference, which is a location
// rather than a description of what is wrong.
func pathLike(text string) bool {
	if strings.ContainsAny(text, " \t") {
		return false
	}
	return strings.ContainsAny(text, "/.:")
}

// firstProse returns the first line that reads like a sentence rather than a
// severity label or a path. Macroscope's summary is its third line.
func firstProse(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "<") || strings.HasPrefix(line, "#") {
			continue
		}
		if usableTitle(line) != "" {
			return line
		}
	}
	return ""
}

// headingOf returns the first Markdown heading's text.
func headingOf(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "#") {
			return strings.TrimSpace(strings.TrimLeft(trimmed, "# "))
		}
	}
	return ""
}

func cleanTitle(title string) string {
	// TitleOf caps at 180 BYTES, which can leave a half-written rune; JSON then
	// renders it as a replacement character.
	title = strings.ToValidUTF8(title, "")
	title = strings.Join(strings.Fields(markup.ReplaceAllString(title, " ")), " ")
	if trimmed := strings.TrimSpace(rubric.ReplaceAllString(title, "")); trimmed != "" {
		title = trimmed
	}
	// By runes: cutting bytes can split a multi-byte character, and the invalid
	// UTF-8 that leaves becomes a replacement character in the JSON.
	if runes := []rune(title); len(runes) > 120 {
		title = string(runes[:117]) + "..."
	}
	return strings.TrimSpace(title)
}
