package thinking

import (
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name string
		body map[string]interface{}
		want Config
	}{
		{"nil", map[string]interface{}{}, Config{}},
		{"bool true", map[string]interface{}{"thinking": true}, Config{Enabled: true, Type: "enabled", Budget: 20000, Effort: "high"}},
		{"bool false", map[string]interface{}{"thinking": false}, Config{}},
		{"string enabled", map[string]interface{}{"thinking": "enabled"}, Config{Enabled: true, Type: "enabled", Budget: 20000, Effort: "high"}},
		{"string adaptive", map[string]interface{}{"thinking": "adaptive"}, Config{Enabled: true, Type: "adaptive", Budget: 20000, Effort: "high"}},
		{"dict enabled with budget", map[string]interface{}{"thinking": map[string]interface{}{"type": "enabled", "budget_tokens": float64(10000)}}, Config{Enabled: true, Type: "enabled", Budget: 10000, Effort: "high"}},
		{"dict budget clamped", map[string]interface{}{"thinking": map[string]interface{}{"type": "enabled", "budget_tokens": float64(99999)}}, Config{Enabled: true, Type: "enabled", Budget: MaxBudgetTokens, Effort: "high"}},
		{"adaptive with effort", map[string]interface{}{"thinking": "adaptive", "output_config": map[string]interface{}{"effort": "low"}}, Config{Enabled: true, Type: "adaptive", Budget: 20000, Effort: "low"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseConfig(tt.body)
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGenerateHint(t *testing.T) {
	tests := []struct {
		cfg  Config
		want string
	}{
		{Config{}, ""},
		{Config{Enabled: true, Type: "enabled", Budget: 20000}, "<thinking_mode>enabled</thinking_mode><max_thinking_length>20000</max_thinking_length>"},
		{Config{Enabled: true, Type: "adaptive", Effort: "medium"}, "<thinking_mode>adaptive</thinking_mode><thinking_effort>medium</thinking_effort>"},
	}
	for _, tt := range tests {
		got := GenerateHint(tt.cfg)
		if got != tt.want {
			t.Errorf("GenerateHint(%+v) = %q, want %q", tt.cfg, got, tt.want)
		}
	}
}

func TestInjectHint(t *testing.T) {
	cfg := Config{Enabled: true, Type: "enabled", Budget: 20000}
	result := InjectHint("You are helpful.", cfg)
	if !strings.HasPrefix(result, "<thinking_mode>") {
		t.Errorf("expected hint prefix, got: %s", result)
	}
	if !strings.Contains(result, "You are helpful.") {
		t.Errorf("expected original prompt preserved")
	}
	// Should not double-inject
	result2 := InjectHint(result, cfg)
	if result2 != result {
		t.Errorf("double injection detected")
	}
}

func TestParserBasic(t *testing.T) {
	p := NewParser()
	segments := p.Push("<thinking>hello world</thinking>\nactual text")
	segments = append(segments, p.Flush()...)

	var thinkText, textText string
	for _, s := range segments {
		if s.Type == SegmentThinking {
			thinkText += s.Text
		} else {
			textText += s.Text
		}
	}
	if thinkText != "hello world" {
		t.Errorf("thinking = %q, want %q", thinkText, "hello world")
	}
	if strings.TrimSpace(textText) != "actual text" {
		t.Errorf("text = %q, want %q", strings.TrimSpace(textText), "actual text")
	}
	if !p.HasExtractedThinking() {
		t.Error("expected HasExtractedThinking = true")
	}
}

func TestParserNoThinking(t *testing.T) {
	p := NewParser()
	segments := p.Push("just normal text")
	segments = append(segments, p.Flush()...)

	for _, s := range segments {
		if s.Type == SegmentThinking {
			t.Errorf("unexpected thinking segment: %q", s.Text)
		}
	}
	if p.IsThinkingMode() {
		t.Error("should not be in thinking mode")
	}
}

func TestParserCrossChunk(t *testing.T) {
	p := NewParser()
	var all []Segment
	all = append(all, p.Push("<think")...)
	all = append(all, p.Push("ing>my thoughts</thi")...)
	all = append(all, p.Push("nking>\ntext here")...)
	all = append(all, p.Flush()...)

	var thinkText, textText string
	for _, s := range all {
		if s.Type == SegmentThinking {
			thinkText += s.Text
		} else {
			textText += s.Text
		}
	}
	if thinkText != "my thoughts" {
		t.Errorf("thinking = %q, want %q", thinkText, "my thoughts")
	}
	if strings.TrimSpace(textText) != "text here" {
		t.Errorf("text = %q, want %q", strings.TrimSpace(textText), "text here")
	}
}

func TestParserQuotedTag(t *testing.T) {
	p := NewParser()
	segments := p.Push("<thinking>use `</thinking>` to close</thinking>\ndone")
	segments = append(segments, p.Flush()...)

	var thinkText string
	for _, s := range segments {
		if s.Type == SegmentThinking {
			thinkText += s.Text
		}
	}
	if !strings.Contains(thinkText, "`</thinking>`") {
		t.Errorf("quoted tag should be preserved in thinking, got: %q", thinkText)
	}
}

func TestParserUnclosed(t *testing.T) {
	p := NewParser()
	segments := p.Push("<thinking>unclosed thinking content")
	segments = append(segments, p.Flush()...)

	var thinkText string
	for _, s := range segments {
		if s.Type == SegmentThinking {
			thinkText += s.Text
		}
	}
	if thinkText != "unclosed thinking content" {
		t.Errorf("unclosed thinking = %q", thinkText)
	}
}
