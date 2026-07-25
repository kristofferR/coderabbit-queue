package dialect

import (
	"regexp"
	"strings"
)

// Macroscope wording. Verified against the live corpus in
// testdata/macroscope/ (pingdotgg/t3code PRs): it auto-reviews every push,
// carries findings as inline comments on empty-body COMMENTED reviews, EDITS a
// finding comment to append "✅ Resolved in <sha>" when it sees a fix (it never
// resolves threads — the edit IS its resolution), posts a per-round
// "Approvability" verdict issue comment, and reports through check runs (app
// slug "macroscopeapp") of which only the Correctness Check can mean clean.

// MacroscopeLogin is the canonical Macroscope GitHub app login.
const MacroscopeLogin = "macroscopeapp[bot]"

const (
	macroscopeCheckPrefix      = "Macroscope - "
	macroscopeCorrectnessCheck = "Macroscope - Correctness Check"
)

var (
	// macroscopeIgnoreRE matches the hidden marker Macroscope opens every
	// comment with.
	macroscopeIgnoreRE = regexp.MustCompile(`<!--\s*MURMUR_IGNORE\s*-->`)
	// macroscopeResolvedRE matches the settled-marker line Macroscope edits
	// into a finding comment once it considers it addressed: "✅ Resolved in
	// <full-sha>" for a fix, "No longer relevant as of <full-sha>" when the
	// code moved on. Both mean the finding is settled.
	macroscopeResolvedRE = regexp.MustCompile(`(?i)(?:✅\s*Resolved in|No longer relevant as of)\s*\x60?([0-9a-fA-F]{7,40})\x60?`)
	// macroscopeVerdictRE captures the Approvability verdict text.
	macroscopeVerdictRE = regexp.MustCompile(`(?i)\*\*Verdict:\*\*\s*([^\n]+)`)
	// macroscopeErrorRE matches the Correctness Check title Macroscope publishes
	// when its own run blew up ("Macroscope encountered an error while reviewing
	// `<sha>`.").
	macroscopeErrorRE = regexp.MustCompile(`(?i)encountered an error while reviewing`)
)

func IsMacroscopeBot(login string) bool {
	return NormalizeBotName(login) == NormalizeBotName(MacroscopeLogin)
}

// IsMacroscopeComment reports whether a body carries Macroscope's hidden
// marker (every comment it posts does).
func IsMacroscopeComment(body string) bool {
	return macroscopeIgnoreRE.MatchString(body)
}

// MacroscopeResolvedInSHA extracts the commit from an edited finding comment's
// "✅ Resolved in <sha>" marker, or "" when the finding is still open.
func MacroscopeResolvedInSHA(body string) string {
	match := macroscopeResolvedRE.FindStringSubmatch(body)
	if len(match) == 2 {
		return strings.ToLower(match[1])
	}
	return ""
}

// MacroscopeApproved parses the Approvability verdict: true for "Approved",
// false for "Needs human review" (or any other non-approved verdict), nil when
// the text carries no verdict at all.
func MacroscopeApproved(text string) *bool {
	if !strings.Contains(text, "#### Approvability") {
		return nil
	}
	match := macroscopeVerdictRE.FindStringSubmatch(text)
	if match == nil {
		return nil
	}
	approved := strings.HasPrefix(strings.ToLower(strings.TrimSpace(match[1])), "approved")
	return &approved
}

// ClassifyMacroscopeComment classifies one of Macroscope's own issue comments:
// the per-round Approvability verdict (EvCoVerdict, informational only — it
// never gates convergence), or any other MURMUR_IGNORE-marked notice
// (EvCoNotice). Unmarked comments stay EvOther.
func ClassifyMacroscopeComment(body string) CoEvent {
	if !IsMacroscopeComment(body) {
		return CoEvent{Kind: EvOther}
	}
	if approved := MacroscopeApproved(body); approved != nil {
		return CoEvent{Kind: EvCoVerdict, Approved: approved}
	}
	return CoEvent{Kind: EvCoNotice}
}

// ClassifyMacroscopeCheck classifies one check run owned by the
// "macroscopeapp" app. Any "Macroscope - <Name>" check is Macroscope's (repos
// add custom ones whose output titles can be garbled — classify by name prefix
// only, never title).
//
// Only the Correctness Check is Macroscope's REVIEW. It publishes Approvability
// and repo-defined custom checks beside it, and those routinely complete first;
// treating them as completion evidence lets the round converge while the
// correctness review is still running and its findings have not landed. They
// are auxiliary: participation, never completion.
func ClassifyMacroscopeCheck(name, title, summary, status, conclusion string) CheckVerdict {
	if !strings.HasPrefix(name, macroscopeCheckPrefix) {
		return CheckUnrelated
	}
	if name != macroscopeCorrectnessCheck {
		// Auxiliary whether running or finished. Reporting a running auxiliary
		// check as CheckInProgress would let it suppress the trigger and engage
		// the completion gate it can never satisfy, stranding the round.
		return CheckAuxiliary
	}
	if !strings.EqualFold(status, "completed") {
		return CheckInProgress
	}
	if checkRunFailed(conclusion) {
		return CheckFailed
	}
	// Macroscope reports its own crashes as `neutral`, which is otherwise an
	// ordinary completed review conclusion (a real "1 issue identified" round
	// concludes neutral too) — so the title, not the conclusion, is what says
	// the review did not happen. A nudge is the right response.
	if macroscopeErrorRE.MatchString(title) {
		return CheckFailed
	}
	// Correctness concludes `skipped` for two very different things, and the
	// conclusion alone cannot tell them apart:
	//
	//   "No code objects were reviewed."  — it ran and had nothing to analyse.
	//       A clean verdict; failing it would strand docs-only PRs.
	//   "Review skipped — billing issue"  — the workspace cannot review at all.
	//       Never review evidence, and re-triggering cannot fix billing, so it
	//       is CheckUnable (suppress the nudge, disengage the gate).
	//
	// Anything else is an unrecognised non-delivery: fail closed with
	// CheckFailed. Falling through to CheckDone let a skipped run — which has no
	// threads and so no findings to gate on — mark a required Macroscope as
	// having reviewed the head, converging the round with no review at all.
	if strings.EqualFold(strings.TrimSpace(conclusion), "skipped") {
		switch {
		case macroscopeSaysAny(title, summary, "no code objects were reviewed"):
			return CheckDoneClean
		case macroscopeSaysAny(title, summary, "billing issue"):
			return CheckUnable
		}
		return CheckFailed
	}
	if macroscopeSaysAny(title, summary, "no issues identified") {
		return CheckDoneClean
	}
	return CheckDone
}

// macroscopeSaysAny reports whether a check run's title or summary carries
// phrase (normalized), the two places Macroscope states a run's outcome.
func macroscopeSaysAny(title, summary, phrase string) bool {
	return strings.Contains(NormalizeReviewText(title), phrase) ||
		strings.Contains(NormalizeReviewText(summary), phrase)
}
