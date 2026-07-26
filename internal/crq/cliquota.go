package crq

import (
	"context"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

// CLIQuotaResult reports what RecordCLIQuota did, so the caller can say so
// instead of failing silently.
type CLIQuotaResult struct {
	Applied bool       `json:"applied"`
	Reason  string     `json:"reason,omitempty"`
	Until   *time.Time `json:"blocked_until,omitempty"`
}

// IsCLIAccountBlock reports whether a preflight run ended in an account-quota
// block, so a caller can decide whether sharing it is even relevant.
func IsCLIAccountBlock(report PreflightReport) bool {
	return dialect.IsCLIRateLimit(report.ErrorType)
}

// RecordCLIQuota folds a rate limit the LOCAL CodeRabbit CLI reported into the
// shared account quota.
//
// The CLI spends the same account-wide review budget the queue exists to
// serialize, so when it refuses locally that is direct evidence about the very
// thing crq otherwise discovers by posting `@coderabbitai rate limit` on a
// calibration PR and reading the reply. This costs no probe comment, no
// calibration-thread growth, and no GitHub round trip beyond the state write —
// and it is fresher, because the CLI answers now rather than whenever CodeRabbit
// gets round to replying.
//
// It is deliberately best-effort and never fatal: preflight is a local command
// that must keep working with no crq config and no GitHub token at all.
//
// Two guards matter:
//
//   - **The account must match.** The CLI can be authenticated to a different
//     CodeRabbit organisation than the one crq queues for, and applying that
//     block would stall reviews for an account that is not limited.
//   - **A block is never shortened.** A standing window from a PR comment is
//     authoritative about that account; a local reading may be a different,
//     narrower limit, so it can only ever extend.
func (s *Service) RecordCLIQuota(ctx context.Context, report PreflightReport, cliOrg string) (CLIQuotaResult, error) {
	if !dialect.IsCLIRateLimit(report.ErrorType) {
		return CLIQuotaResult{Reason: "the cli reported no account block"}, nil
	}
	if err := s.cfg.RequireState(); err != nil {
		return CLIQuotaResult{Reason: "no crq state configured, so there is no shared quota to update"}, nil
	}
	if !s.cliOrgMatches(cliOrg) {
		return CLIQuotaResult{
			Reason: "the coderabbit cli is authenticated to " + orDash(cliOrg) +
				", which is not the account crq queues for (" + strings.Join(s.cfg.Scope, ",") + ")",
		}, nil
	}

	now := s.clock()
	until := dialect.ParseCLIWaitTime(report.RetryAfter, now)
	if until == nil {
		// The CLI said "blocked" without a window crq could read. Waiting the
		// conservative fallback is right: treating an unreadable window as "not
		// blocked" is what let the daemon re-fire every couple of minutes against
		// a limit measured in tens of minutes.
		fallback := now.Add(s.cfg.RateLimitFallback)
		until = &fallback
	}

	result := CLIQuotaResult{Until: until}
	if s.cfg.DryRun {
		result.Reason = "dry run: would record the block"
		return result, nil
	}

	// Update swallows ErrNoChange, so whether anything was written has to be
	// recorded by the mutation itself. It is assigned on every attempt because a
	// CAS conflict runs the closure again.
	applied := false
	state, err := s.store.Update(ctx, func(st *State) error {
		if st.Account.BlockedUntil != nil && !until.After(*st.Account.BlockedUntil) {
			applied = false
			return ErrNoChange
		}
		applied = true
		st.Account.BlockedUntil = until
		st.Account.CheckedAt = &now
		st.Account.Source = "coderabbit-cli"
		st.Account.Scope = strings.Join(s.cfg.Scope, ",")
		// A local reading says nothing about how many PR reviews are left, and
		// leaving a stale count beside a fresh block would read as authoritative.
		st.Account.Remaining = nil
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Applied = applied
	if !applied {
		result.Reason = "a longer account block is already recorded"
		result.Until = state.Account.BlockedUntil
		return result, nil
	}
	s.sync(ctx, state)
	return result, nil
}

// cliOrgMatches reports whether the CLI's current organisation is the account
// crq queues for. An empty org fails closed: without knowing whose limit this is,
// applying it fleet-wide is the more expensive mistake.
func (s *Service) cliOrgMatches(cliOrg string) bool {
	org := strings.ToLower(strings.TrimSpace(cliOrg))
	if org == "" {
		return false
	}
	for _, scope := range s.cfg.Scope {
		if strings.EqualFold(strings.TrimSpace(scope), org) {
			return true
		}
	}
	return strings.EqualFold(ownerOf(s.cfg.GateRepo), org)
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "an unknown organisation"
	}
	return value
}
