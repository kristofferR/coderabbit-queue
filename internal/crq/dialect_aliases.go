package crq

import (
	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// Aliases for the bot-text helpers that moved to internal/dialect. They keep
// existing call sites and tests stable while the refactor lands package by
// package; new code should call dialect directly. Deleted once the engine
// extraction rewrites the callers.

type Finding = dialect.Finding

var (
	severityOf                      = dialect.SeverityOf
	rankSeverity                    = dialect.RankSeverity
	titleOf                         = dialect.TitleOf
	stripMarkdownQuote              = dialect.StripMarkdownQuote
	compactReviewBody               = dialect.CompactReviewBody
	normalizeReviewText             = dialect.NormalizeReviewText
	looksLikePath                   = dialect.LooksLikePath
	normalizeBotName                = dialect.NormalizeBotName
	inBots                          = dialect.InBots
	botSet                          = dialect.BotSet
	isCodexBot                      = dialect.IsCodexBot
	hasCodexBot                     = dialect.HasCodexBot
	isNonActionableText             = dialect.IsNonActionableText
	isActionableFinding             = dialect.IsActionableFinding
	isNoActionReviewCompletion      = dialect.IsNoActionReviewCompletion
	isCodexNoActionReviewCompletion = dialect.IsCodexNoActionReviewCompletion
	codexReviewedCommitSHA          = dialect.CodexReviewedCommitSHA
	shaPrefixMatch                  = dialect.SHAPrefixMatch
	shortOID                        = dialect.ShortOID
	parseAvailableIn                = dialect.ParseAvailableIn
	parseQuota                      = dialect.ParseQuota
	parseRemainingReviews           = dialect.ParseRemainingReviews
)

// parseReviewBodyFindings adapts a GitHub review to the dialect parsers, which
// take only the metadata they attach to findings.
func parseReviewBodyFindings(review Review, bot string) []Finding {
	return dialect.ParseReviewBodyFindings(review.Body, dialect.ReviewMeta{
		ID:          review.ID,
		CommitID:    review.CommitID,
		HTMLURL:     review.HTMLURL,
		SubmittedAt: review.SubmittedAt,
	}, bot)
}
