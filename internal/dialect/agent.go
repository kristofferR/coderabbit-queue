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
	text := strings.ToLower(string(log))
	var decoded []any
	for _, line := range strings.Split(string(log), "\n") {
		var value any
		if json.Unmarshal([]byte(line), &value) == nil {
			decoded = append(decoded, value)
		}
	}
	found := false
	for _, value := range decoded {
		if hasStringMember(value, "error", "rate_limit") ||
			hasStringMember(value, "type", "rate_limit_error") ||
			hasStringMember(value, "type", "overloaded_error") {
			found = true
			break
		}
	}
	if !found {
		return AgentFailure{}
	}

	retryAt := now.UTC().Add(15 * time.Minute)
	// Claude's stream-json error includes an epoch-seconds resetsAt member.
	// Decode each line when possible, then use a narrow textual fallback for a
	// partially-written final line.
	for _, value := range decoded {
		if unix, ok := findUnixMember(value, "resetsAt"); ok {
			retryAt = time.Unix(unix, 0).UTC()
		}
	}
	if i := strings.Index(text, `"resetsat":`); i >= 0 {
		value := strings.TrimLeft(text[i+len(`"resetsat":`):], " \t")
		end := 0
		for end < len(value) && value[end] >= '0' && value[end] <= '9' {
			end++
		}
		if unix, err := strconv.ParseInt(value[:end], 10, 64); end > 0 && err == nil {
			retryAt = time.Unix(unix, 0).UTC()
		}
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

func hasStringMember(value any, key, want string) bool {
	switch value := value.(type) {
	case map[string]any:
		for k, child := range value {
			if strings.EqualFold(k, key) {
				if text, ok := child.(string); ok && strings.EqualFold(text, want) {
					return true
				}
			}
			if hasStringMember(child, key, want) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if hasStringMember(child, key, want) {
				return true
			}
		}
	}
	return false
}

func findUnixMember(value any, key string) (int64, bool) {
	switch value := value.(type) {
	case map[string]any:
		for k, child := range value {
			if strings.EqualFold(k, key) {
				switch n := child.(type) {
				case float64:
					return int64(n), true
				case string:
					unix, err := strconv.ParseInt(n, 10, 64)
					return unix, err == nil
				}
			}
			if unix, ok := findUnixMember(child, key); ok {
				return unix, true
			}
		}
	case []any:
		for _, child := range value {
			if unix, ok := findUnixMember(child, key); ok {
				return unix, true
			}
		}
	}
	return 0, false
}
