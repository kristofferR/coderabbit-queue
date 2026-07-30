package state

import (
	"encoding/json"
	"fmt"
	"strings"
)

// migrateV5State translates v5's flat fleet policy map into FleetDefaults.
// The "fleet" member kept its JSON name in v5, so decoding it directly would
// either discard the old policy or fail on values such as cobots, whose shape
// changed from a comma-separated string to an array.
func migrateV5State(raw []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	fleetRaw, ok := root["fleet"]
	if !ok || string(fleetRaw) == "null" {
		return raw, nil
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(fleetRaw, &legacy); err != nil {
		return nil, fmt.Errorf("decode v5 fleet policy: %w", err)
	}

	next := make(map[string]any)
	env := make(map[string]json.RawMessage)
	if valueRaw, ok := legacy["env"]; ok {
		// Some v5 writers already emitted FleetDefaults while others still
		// emitted the flat policy. Start with the nested shape so flat legacy
		// keys below deterministically win when both spell the same setting.
		if err := json.Unmarshal(valueRaw, &env); err != nil {
			return nil, fmt.Errorf("decode v5 fleet env: %w", err)
		}
	}
	for key, valueRaw := range legacy {
		if key == "env" {
			continue
		}
		var value string
		if err := json.Unmarshal(valueRaw, &value); err != nil {
			// V4 values were strings. Preserve an unexpected future member
			// verbatim so the migration is no more destructive than tolerant
			// decoding of an unknown v5 field.
			next[key] = valueRaw
			continue
		}
		switch key {
		case "required-bots":
			next["required"] = splitLegacyList(value)
			next["set_required"] = true
		case "cobots":
			next["cobots"] = splitLegacyList(value)
			next["set_cobots"] = true
		case "min-interval":
			next["min_interval"] = strings.TrimSpace(value)
		default:
			if envKey, ok := legacyFleetEnvKey(key); ok {
				encodedValue, err := json.Marshal(strings.TrimSpace(value))
				if err != nil {
					return nil, fmt.Errorf("encode v5 fleet env %s: %w", envKey, err)
				}
				env[envKey] = encodedValue
			} else {
				next[key] = value
			}
		}
	}
	if len(env) > 0 {
		next["env"] = env
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	root["fleet"] = encoded
	return json.Marshal(root)
}

func splitLegacyList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func legacyFleetEnvKey(key string) (string, bool) {
	keys := map[string]string{
		"scope":                 "CRQ_SCOPE",
		"repos":                 "CRQ_REPOS",
		"exclude":               "CRQ_EXCLUDE",
		"feedback-bots":         "CRQ_FEEDBACK_BOTS",
		"inflight-timeout":      "CRQ_INFLIGHT_TIMEOUT",
		"rate-limit-fallback":   "CRQ_RL_FALLBACK",
		"calibrate-ttl":         "CRQ_CALIBRATE_TTL",
		"settle":                "CRQ_SETTLE",
		"skip-marker":           "CRQ_AUTOREVIEW_SKIP_MARKER",
		"skip-authors":          "CRQ_AUTOREVIEW_SKIP_AUTHORS",
		"rate-limit-co-degrade": "CRQ_RL_CO_DEGRADE",
	}
	if envKey, ok := keys[key]; ok {
		return envKey, true
	}
	if !strings.HasPrefix(key, "cobot-") {
		return "", false
	}
	rest := strings.TrimPrefix(key, "cobot-")
	name, suffix, ok := strings.Cut(rest, "-")
	if !ok || name == "" {
		return "", false
	}
	switch suffix {
	case "trigger", "cmd", "grace":
		return "CRQ_COBOT_" + strings.ToUpper(name) + "_" + strings.ToUpper(suffix), true
	default:
		return "", false
	}
}
