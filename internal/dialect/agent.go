package dialect

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// AgentFailure describes a provider/model failure that should be retried on a
// fallback rather than counted as a failed attempt to change code.
type AgentFailure struct {
	Unavailable bool
	RetryAt     time.Time
	Reason      string
}

// ClassifyAgentFailure recognizes machine-readable outage responses emitted by
// supported coding agents. The transcript also contains repository-controlled
// command output, test failures, source code, and the review findings
// themselves, so free-text markers are not trustworthy evidence. Unknown
// failures stay ordinary fix failures: refunding a genuine bad fix forever
// would remove the loop's safety bound.
func ClassifyAgentFailure(log []byte, now time.Time) AgentFailure {
	found := false
	var resetAt int64
	for _, line := range strings.Split(string(log), "\n") {
		var event agentEvent
		if json.Unmarshal([]byte(line), &event) != nil || !event.providerUnavailable() {
			continue
		}
		found = true
		if unix, ok := event.resetUnix(); ok {
			resetAt = unix
		}
	}
	if !found {
		return AgentFailure{}
	}

	retryAt := now.UTC().Add(15 * time.Minute)
	if resetAt != 0 {
		retryAt = time.Unix(resetAt, 0).UTC()
	}
	if !retryAt.After(now) {
		retryAt = now.UTC().Add(5 * time.Minute)
	}
	// Agent output is influenced by repository code. Never let a bogus reset
	// timestamp park a model for this head indefinitely.
	if latest := now.UTC().Add(7 * 24 * time.Hour); retryAt.After(latest) {
		retryAt = latest
	}
	return AgentFailure{
		Unavailable: true,
		RetryAt:     retryAt,
		Reason:      "provider or model temporarily unavailable",
	}
}

// agentEvent is deliberately only the outer protocol envelope. Repository
// output is embedded inside assistant/user events and may itself contain
// outage-shaped JSON; recursively scanning it would let a test refund a failed
// fix and bypass the per-head attempt bound.
type agentEvent struct {
	Type     string          `json:"type"`
	Error    json.RawMessage `json:"error"`
	ResetsAt json.RawMessage `json:"resetsAt"`
}

func (e agentEvent) providerUnavailable() bool {
	if e.Type != "result" && e.Type != "error" {
		return false
	}
	var direct string
	if json.Unmarshal(e.Error, &direct) == nil {
		return strings.EqualFold(direct, "rate_limit")
	}
	var nested struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(e.Error, &nested) != nil {
		return false
	}
	return strings.EqualFold(nested.Type, "rate_limit_error") ||
		strings.EqualFold(nested.Type, "overloaded_error")
}

func (e agentEvent) resetUnix() (int64, bool) {
	var number json.Number
	if json.Unmarshal(e.ResetsAt, &number) == nil {
		unix, err := strconv.ParseInt(string(number), 10, 64)
		return unix, err == nil
	}
	var text string
	if json.Unmarshal(e.ResetsAt, &text) == nil {
		unix, err := strconv.ParseInt(text, 10, 64)
		return unix, err == nil
	}
	return 0, false
}
