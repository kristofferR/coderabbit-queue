package dialect

import (
	"regexp"
	"strings"
)

// markup matches the wrappers a bot puts AROUND its title: badge images, HTML
// tags, and emphasis runs that delimit a span. Left in, a listing reads as
// `![P2 Badge](https://img.shields.io/...)` instead of what the finding says.
//
// Emphasis is matched only where it delimits — at a boundary — so a literal
// underscore inside a word survives. Stripping every `_` turned "Handle user_id"
// into "Handle user id", which is a different identifier.
var markup = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)|<[^>]+>|(^|\s)[*_` + "`" + `#]+|[*_` + "`" + `#]+(\s|$)`)

// rubric matches CodeRabbit's fixed severity header — "🎯 Functional
// Correctness | 🟡 Minor | ⚡ Quick win" — which is the same on every finding
// and displaces the part that differs. It matches the SHAPE rather than any
// pipe, because a title may contain one: "Support A | B configuration" must not
// become "B configuration".
var rubric = regexp.MustCompile(`^[^|]*\b(Correctness|Maintainability|Security|Performance|Reliability|Quality)\b[^|]*\|[^|]*\|[^|]*\|?\s*`)

// severityOnly matches a title that is just a severity label — "High Severity",
// "🟠 High", "Critical" — which is what the first bold span is for Bugbot and
// Macroscope. It carries no information a severity field does not already hold.
var severityOnly = regexp.MustCompile(`(?i)^\W*(critical|high|medium|low|major|minor|blocker|trivial|info)(\s+severity)?\W*$`)

// ThreadTitle reduces a review comment to one readable line, per bot.
//
// Every bot wraps its summary differently: CodeRabbit prefixes a fixed severity
// rubric, Codex leads with a badge image, Bugbot opens with a `###` heading
// followed by a bold severity, and Macroscope starts with an emoji severity and
// the path. Taking the first bold span works for two of them and reports "High
// Severity" for the others.
//
// It lives here because this is where bot wording lives; a listing in the
// orchestration layer must not have to know how a bot writes a heading.
func ThreadTitle(author, body string) string {
	candidates := []string{TitleFromDetailedBlock(body)}
	if co, ok := CoReviewerByName(author); ok && co.Name != "" {
		// A heading, where the bots that lead with a severity label put the
		// actual summary.
		candidates = append([]string{headingOf(body)}, candidates...)
	}
	candidates = append(candidates, TitleOf(body))

	for _, candidate := range candidates {
		title := cleanTitle(candidate)
		if title != "" && !severityOnly.MatchString(title) {
			return title
		}
	}
	// Everything looked like a severity label; the least bad answer is the
	// first non-empty one rather than nothing at all.
	for _, candidate := range candidates {
		if title := cleanTitle(candidate); title != "" {
			return title
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
