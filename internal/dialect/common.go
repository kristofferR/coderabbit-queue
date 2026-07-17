package dialect

import (
	"regexp"
	"strings"
)

var (
	boldTitleRE   = regexp.MustCompile(`(?m)^\*\*([^*\n]+)\*\*`)
	crCommentRE   = regexp.MustCompile(`<!--\s*cr-comment:v1:([a-f0-9]+)\s*-->`)
	htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
	rootFileRE    = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
)

func SeverityOf(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "critical"), strings.Contains(lower, "🔴"):
		return "critical"
	case strings.Contains(lower, "major"), strings.Contains(lower, "high"), strings.Contains(lower, "🟠"):
		return "major"
	case strings.Contains(lower, "potential issue"), strings.Contains(lower, "medium"), strings.Contains(lower, "🟡"):
		return "potential"
	case strings.Contains(lower, "nitpick"), strings.Contains(lower, "minor"), strings.Contains(lower, "low"), strings.Contains(lower, "🔵"):
		return "minor"
	default:
		return "unknown"
	}
}

func RankSeverity(sev string) int {
	switch sev {
	case "critical":
		return 5
	case "major":
		return 4
	case "potential":
		return 3
	case "minor":
		return 2
	default:
		return 1
	}
}

func TitleOf(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, "#*_` ")
		if line != "" && !strings.HasPrefix(line, "<details") && !strings.HasPrefix(line, "</") {
			if len(line) > 180 {
				line = line[:180]
			}
			return line
		}
	}
	return "Review finding"
}

func TitleFromDetailedBlock(body string) string {
	if match := boldTitleRE.FindStringSubmatch(body); match != nil {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func StripMarkdownQuote(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, " \t")
		// CodeRabbit nests review-body sections (outside-diff-range,
		// duplicates, nitpicks) several blockquote levels deep — strip
		// every leading quote marker, not just the first.
		for strings.HasPrefix(line, ">") {
			line = strings.TrimPrefix(line, ">")
			line = strings.TrimPrefix(line, " ")
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

func CompactReviewBody(body string) string {
	body = crCommentRE.ReplaceAllString(body, "")
	body = htmlCommentRE.ReplaceAllString(body, "")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	var out []string
	skipFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			skipFence = !skipFence
			continue
		}
		if skipFence || strings.HasPrefix(trimmed, "<details") || strings.HasPrefix(trimmed, "</details") ||
			strings.HasPrefix(trimmed, "<summary") || strings.HasPrefix(trimmed, "</summary") ||
			strings.HasPrefix(trimmed, "<blockquote") || strings.HasPrefix(trimmed, "</blockquote") {
			continue
		}
		isRule := len(trimmed) >= 3 && strings.Trim(trimmed, "-_* ") == ""
		if trimmed != "" && !isRule {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, "\n")
}

func LooksLikePath(summary string) bool {
	summary = strings.TrimSpace(summary)
	if summary == "" || strings.Contains(summary, " ") {
		return false
	}
	if strings.Contains(summary, "/") || strings.Contains(summary, ".") {
		return true
	}
	// Root-level files often have neither a slash nor a dot (Dockerfile, Makefile,
	// LICENSE). In a "<file> (N)" detail summary a single filename-safe token is a
	// file, so accept it rather than dropping its findings.
	return rootFileRE.MatchString(summary)
}

func IsNonActionableText(text string) bool {
	if IsCodexNoActionReviewCompletion(text) || IsCodexUsageLimit(text) || IsCodexEnvironmentNotice(text) {
		return true
	}
	text = NormalizeReviewText(text)
	// CodeRabbit appends an "Also applies to: <lines>" trailer to REAL finding
	// bodies listing other affected locations — a substring match on it silently
	// dropped four genuine findings in one review. Only a comment that IS the
	// trailer (an ack pointing at an already-reported location) is noise.
	if strings.HasPrefix(strings.TrimSpace(text), "also applies to:") {
		return true
	}
	nonActionable := []string{
		"lgtm",
		"no issue here",
		"incorrect or invalid review comment",
		"likely an incorrect or invalid review comment",
		"version claim",
		"both referenced files exist",
		"good regression test",
		"already fixed",
		"now fixed",
		"no further action is needed",
		"confirm intended ux",
		"worth confirming",
		"skipped: comment is from another github bot",
	}
	for _, phrase := range nonActionable {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func IsNoActionReviewCompletion(text string) bool {
	text = NormalizeReviewText(text)
	return strings.Contains(text, "no actionable comments were generated in the recent review") ||
		IsCodexNoActionReviewCompletion(text)
}

func NormalizeReviewText(text string) string {
	return strings.NewReplacer("’", "'", "‘", "'").Replace(strings.ToLower(text))
}

func ShortOID(oid string) string {
	if len(oid) >= 9 {
		return oid[:9]
	}
	return oid
}

func BotSet(bots []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, bot := range bots {
		bot = strings.TrimSpace(bot)
		if bot != "" {
			out[bot] = struct{}{}
		}
	}
	return out
}

// InBots matches a comment author against the configured bots, tolerating the
// "[bot]" suffix: GitHub's REST API reports "coderabbitai[bot]" but GraphQL
// (review threads) reports "coderabbitai", and the config may use either form.
// Without this, crq missed every review-thread finding and so never surfaced a
// thread_id to resolve.
func InBots(bots map[string]struct{}, login string) bool {
	if _, ok := bots[login]; ok {
		return true
	}
	stripped := strings.TrimSuffix(login, "[bot]")
	if _, ok := bots[stripped]; ok {
		return true
	}
	_, ok := bots[stripped+"[bot]"]
	return ok
}

func NormalizeBotName(login string) string {
	return strings.TrimSuffix(login, "[bot]")
}

// ParseReviewBodyFindings extracts every finding representable only in a
// review's body text: CodeRabbit's failed-to-post/outside-diff detail blocks,
// its "Prompt for AI agents" block, and Codex's blob-link items.
func ParseReviewBodyFindings(body string, review ReviewMeta, bot string) []Finding {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	clean := StripMarkdownQuote(body)
	out := ParseDetailedReviewFindings(clean, review, bot)
	out = append(out, ParsePromptReviewFindings(clean, review, bot)...)
	out = append(out, ParseCodexReviewFindings(clean, review, bot)...)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
