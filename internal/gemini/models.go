package gemini

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ModelInfo describes a model entry from the live otAQ7b RPC registry or static fallback.
type ModelInfo struct {
	ModelID       string   `json:"modelId"`
	ModelName     string   `json:"modelName"`
	DisplayName   string   `json:"displayName"`
	Aliases       []string `json:"aliases,omitempty"`
	Description   string   `json:"description,omitempty"`
	Capacity      int      `json:"capacity"`
	CapacityField int      `json:"capacityField"`
	ModelNumber   int      `json:"modelNumber"`
}

// QuotaBucket represents one rate-limit/quota bucket returned by the qpEbW RPC.
type QuotaBucket struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	UsagePercentage *float64 `json:"usagePercentage"`
	Total           int      `json:"total"`
	Remaining       int      `json:"remaining"`
	ResetAt         string   `json:"resetAt,omitempty"`
	Unlimited       bool     `json:"unlimited"`
}

// ThinkingLevels maps Gemini Web's 3 thinking levels ("low", "medium", "high") to
// Google's Assistant_Lamda_ThinkingLevel protobuf enum (otAQ7b field 24):
//
//	1 = THINKING_LEVEL_STANDARD ("low", default)
//	4 = THINKING_LEVEL_MEDIUM   ("medium")
//	2 = THINKING_LEVEL_EXTENDED ("high")
var ThinkingLevels = map[string]int{
	"low":    1,
	"medium": 4,
	"high":   2,
}

// SupportedThinkingEfforts lists the 3 valid thinking effort levels in order.
var SupportedThinkingEfforts = []string{"low", "medium", "high"}

var reThinkingCapable = regexp.MustCompile(`(?i)thinking|pro|argon|flash`)

// IsReasoningModel reports whether modelName supports extended thinking/reasoning.
func IsReasoningModel(modelName string) bool {
	return reThinkingCapable.MatchString(modelName)
}

// ComputeCapacity derives the account's capacity and capacityField (12 or 13)
// from otAQ7b's tierFlags (field 16) and capFlags (field 17).
func ComputeCapacity(tierFlags, capFlags []int) (capacity int, capacityField int) {
	has := func(arr []int, v int) bool {
		for _, x := range arr {
			if x == v {
				return true
			}
		}
		return false
	}
	if has(tierFlags, 21) {
		return 1, 13
	}
	if has(tierFlags, 22) {
		return 2, 13
	}
	if has(capFlags, 115) {
		return 4, 12 // Plus
	}
	if has(tierFlags, 16) || has(capFlags, 106) {
		return 3, 12 // Pro
	}
	if has(tierFlags, 8) || has(capFlags, 19) {
		return 2, 12 // Pro
	}
	return 1, 12 // Free
}

// BuildModelHeader constructs the x-goog-ext-525001261-jspb header JSON array
// for a given ModelInfo, thinking level, and f.sid.
func BuildModelHeader(m ModelInfo, thinkLvl int, fSid string) string {
	var arr []any
	if m.CapacityField == 13 {
		arr = []any{
			1, nil, nil, nil, m.ModelID, nil, nil, 0, []int{4, 5, 6, 8},
			nil, nil, nil, m.Capacity, nil, nil, m.ModelNumber, thinkLvl, fSid,
		}
	} else {
		capVal := m.Capacity
		if capVal <= 0 {
			capVal = 1
		}
		numVal := m.ModelNumber
		if numVal <= 0 {
			numVal = 1
		}
		arr = []any{
			1, nil, nil, nil, m.ModelID, nil, nil, 0, []int{4, 5, 6, 8},
			nil, nil, capVal, nil, nil, numVal, thinkLvl, fSid,
		}
	}
	b, _ := json.Marshal(arr)
	return string(b)
}

// knownModelPrefixes lists router/provider prefixes stripped before matching a model name.
var knownModelPrefixes = []string{
	"sentinel/",
	"google/",
	"models/",
	"vx/",
	"vertex/",
}

// ParseModelRequest splits an incoming model identifier (e.g. "sentinel/gemini-4-argon@think=high")
// into its normalized model name ("gemini-4-argon") and optional inline thinking override ("high").
func ParseModelRequest(raw string) (cleanModel string, inlineThink string) {
	s := strings.TrimSpace(raw)
	if idx := strings.LastIndex(strings.ToLower(s), "@think="); idx >= 0 {
		inlineThink = strings.TrimSpace(s[idx+len("@think="):])
		s = strings.TrimSpace(s[:idx])
	}
	lower := strings.ToLower(s)
	for _, p := range knownModelPrefixes {
		if strings.HasPrefix(lower, p) {
			s = strings.TrimSpace(s[len(p):])
			break
		}
	}
	return s, inlineThink
}

// MatchesModel reports whether a ModelInfo matches the requested model string
// by modelName, modelId, displayName, or alias (with optional router prefix or @think= suffix).
func MatchesModel(m ModelInfo, wanted string) bool {
	w, _ := ParseModelRequest(wanted)
	if w == "" {
		return false
	}
	if strings.EqualFold(m.ModelName, w) || strings.EqualFold(m.ModelID, w) || strings.EqualFold(m.DisplayName, w) {
		return true
	}
	for _, a := range m.Aliases {
		if strings.EqualFold(a, w) {
			return true
		}
	}
	return false
}

// IsMatchingServedModel checks whether the upstream frame's answeredModelId /
// answeredModelLabel matches the requested ModelInfo (preventing silent downgrades).
func IsMatchingServedModel(m ModelInfo, servedID, servedLabel string) bool {
	if servedID != "" && m.ModelID != "" {
		if strings.EqualFold(servedID, m.ModelID) {
			return true
		}
	}
	if servedLabel != "" {
		norm := func(s string) string {
			s = strings.TrimSpace(strings.ToLower(s))
			s = strings.TrimPrefix(s, "gemini")
			s = strings.TrimLeft(s, " -")
			return strings.Join(strings.Fields(s), "-")
		}
		stripVer := func(s string) string {
			parts := strings.Split(s, "-")
			var kept []string
			for _, p := range parts {
				if p == "" {
					continue
				}
				isVer := true
				for i := range len(p) {
					if (p[i] < '0' || p[i] > '9') && p[i] != '.' {
						isVer = false
						break
					}
				}
				if !isVer {
					kept = append(kept, p)
				}
			}
			return strings.Join(kept, "-")
		}
		s := norm(servedLabel)
		d := norm(m.DisplayName)
		n := norm(m.ModelName)
		if s != "" && (s == d || s == n) {
			return true
		}
		if servedID == "" || m.ModelID == "" {
			sv := stripVer(s)
			if sv != "" && (sv == stripVer(d) || sv == stripVer(n)) {
				return true
			}
		}
	}
	return false
}

// resolveDefaultThinkingLevel maps a default effort string or enum number to 1 (low), 4 (medium), or 2 (high).
func resolveDefaultThinkingLevel(defaultEffort string) int {
	switch strings.ToLower(strings.TrimSpace(defaultEffort)) {
	case "medium", "4":
		return 4
	case "high", "2":
		return 2
	default:
		return 1 // "low" (default)
	}
}

// ThinkingLevelFor resolves the thinking level from a request body effort value
// and model ID, strictly using Gemini Web's 3 levels: low (1), medium (4), high (2),
// with default low (1).
func ThinkingLevelFor(rawEffort any, modelName, defaultEffort string) int {
	defLvl := resolveDefaultThinkingLevel(defaultEffort)
	if rawEffort == nil || rawEffort == "" {
		if reThinkingCapable.MatchString(modelName) {
			return defLvl
		}
		return 1
	}
	switch v := rawEffort.(type) {
	case string:
		key := strings.ToLower(strings.TrimSpace(v))
		if key == "" {
			if reThinkingCapable.MatchString(modelName) {
				return defLvl
			}
			return 1
		}
		if lvl, exists := ThinkingLevels[key]; exists {
			return lvl
		}
		// Normalize numeric or router-clamped strings into the 3 Gemini levels (low=1, medium=4, high=2)
		switch key {
		case "0", "1", "low", "min", "minimal", "none", "off", "disabled", "false":
			return 1
		case "4", "medium", "med":
			return 4
		case "2", "high", "max", "xhigh", "extended", "enabled", "adaptive", "true":
			return 2
		default:
			return defLvl
		}
	case float64:
		switch int(v) {
		case 4:
			return 4
		case 2:
			return 2
		default:
			return 1
		}
	case int:
		switch v {
		case 4:
			return 4
		case 2:
			return 2
		default:
			return 1
		}
	}
	return defLvl
}
