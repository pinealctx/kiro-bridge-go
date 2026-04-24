package thinking

import (
	"fmt"
	"strings"
)

const (
	DefaultBudgetTokens = 20000
	MaxBudgetTokens     = 24576
	DefaultEffort       = "high"
)

type Config struct {
	Enabled bool
	Type    string // "enabled" or "adaptive"
	Budget  int
	Effort  string // "high", "medium", "low"
}

// ParseConfig extracts thinking configuration from the raw request body.
// Supports: bool, string ("enabled"/"adaptive"), or dict with type/budget_tokens.
func ParseConfig(body map[string]interface{}) Config {
	raw := body["thinking"]
	if raw == nil {
		return Config{}
	}

	switch v := raw.(type) {
	case bool:
		if v {
			return Config{Enabled: true, Type: "enabled", Budget: DefaultBudgetTokens, Effort: DefaultEffort}
		}
		return Config{}
	case string:
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "enabled" || v == "adaptive" {
			effort := parseEffort(body)
			budget := DefaultBudgetTokens
			if v == "enabled" {
				return Config{Enabled: true, Type: "enabled", Budget: budget, Effort: effort}
			}
			return Config{Enabled: true, Type: "adaptive", Budget: budget, Effort: effort}
		}
		return Config{}
	case map[string]interface{}:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		if typ != "enabled" && typ != "adaptive" {
			if budget, ok := toInt(v["budget_tokens"]); ok && budget > 0 {
				typ = "enabled"
			} else {
				return Config{}
			}
		}
		budget := DefaultBudgetTokens
		if b, ok := toInt(v["budget_tokens"]); ok && b > 0 {
			budget = b
			if budget > MaxBudgetTokens {
				budget = MaxBudgetTokens
			}
		}
		effort := parseEffort(body)
		return Config{Enabled: true, Type: typ, Budget: budget, Effort: effort}
	}
	return Config{}
}

func parseEffort(body map[string]interface{}) string {
	for _, key := range []string{"output_config", "outputConfig"} {
		if oc, ok := body[key].(map[string]interface{}); ok {
			if e, ok := oc["effort"].(string); ok {
				e = strings.ToLower(strings.TrimSpace(e))
				if e == "high" || e == "medium" || e == "low" {
					return e
				}
			}
		}
	}
	return DefaultEffort
}

// GenerateHint returns the XML hint string for injection into system prompt.
func GenerateHint(cfg Config) string {
	if !cfg.Enabled {
		return ""
	}
	if cfg.Type == "adaptive" {
		return fmt.Sprintf("<thinking_mode>adaptive</thinking_mode><thinking_effort>%s</thinking_effort>", cfg.Effort)
	}
	return fmt.Sprintf("<thinking_mode>enabled</thinking_mode><max_thinking_length>%d</max_thinking_length>", cfg.Budget)
}

// InjectHint prepends the thinking hint to the system prompt if not already present.
func InjectHint(systemPrompt string, cfg Config) string {
	if !cfg.Enabled {
		return systemPrompt
	}
	if strings.Contains(systemPrompt, "<thinking_mode>") {
		return systemPrompt
	}
	hint := GenerateHint(cfg)
	if systemPrompt == "" {
		return hint
	}
	return hint + "\n\n" + systemPrompt
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}
