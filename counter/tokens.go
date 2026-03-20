package counter

import (
	"encoding/json"
	"unicode"
)

// isCJK returns true if the rune is in a CJK unicode range.
func isCJK(r rune) bool {
	return (r >= 0x4e00 && r <= 0x9fff) ||
		(r >= 0x3400 && r <= 0x4dbf) ||
		(r >= 0x3000 && r <= 0x303f) ||
		(r >= 0xff00 && r <= 0xffef) ||
		(r >= 0x3040 && r <= 0x30ff) ||
		(r >= 0xac00 && r <= 0xd7af)
}

// EstimateTokens estimates token count using CJK heuristic.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	cjk := 0
	total := 0
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if isCJK(r) {
			cjk++
		}
	}
	result := float64(cjk)/1.5 + float64(total-cjk)/4.0 + 0.5
	if result < 1 {
		return 1
	}
	return int(result)
}

// EstimateMessagesTokens estimates total tokens for a list of messages.
func EstimateMessagesTokens(messages []map[string]interface{}) int {
	total := 3 // base overhead
	for _, msg := range messages {
		total += 4 // per-message overhead
		if content, ok := msg["content"]; ok {
			switch v := content.(type) {
			case string:
				total += EstimateTokens(v)
			case []interface{}:
				for _, block := range v {
					if b, ok := block.(map[string]interface{}); ok {
						if t, ok := b["text"].(string); ok {
							total += EstimateTokens(t)
						}
					}
				}
			}
		}
		// Tool calls
		if toolCalls, ok := msg["tool_calls"].([]interface{}); ok {
			for _, tc := range toolCalls {
				if t, ok := tc.(map[string]interface{}); ok {
					if fn, ok := t["function"].(map[string]interface{}); ok {
						total += EstimateTokens(fn["name"].(string))
						if args, ok := fn["arguments"].(string); ok {
							total += EstimateTokens(args)
						}
					}
				}
			}
		}
	}
	total += 3 // assistant priming
	return total
}

// EstimateMessagesTokensJSON estimates tokens from JSON-serializable messages.
func EstimateMessagesTokensJSON(messages interface{}) int {
	b, err := json.Marshal(messages)
	if err != nil {
		return 0
	}
	return EstimateTokens(string(b))
}
