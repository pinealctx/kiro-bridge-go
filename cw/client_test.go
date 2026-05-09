package cw

import (
	"testing"

	"github.com/pinealctx/kiro-bridge-go/config"
)

func TestResolveModelUsesExplicitMappingFirst(t *testing.T) {
	c := &Client{cfg: &config.Config{
		ModelMap: map[string]string{
			"claude-sonnet-4-7": "custom-backend-model",
		},
	}}

	got := c.resolveModel("claude-sonnet-4-7")
	if got != "custom-backend-model" {
		t.Fatalf("resolveModel() = %q, want %q", got, "custom-backend-model")
	}
}

func TestResolveModelNormalizesClaudeFallback(t *testing.T) {
	c := &Client{cfg: &config.Config{ModelMap: map[string]string{}}}

	tests := map[string]string{
		"claude-opus-4-7":          "claude-opus-4.7",
		"claude-opus-4-8":          "claude-opus-4.8",
		"claude-opus-5-0":          "claude-opus-5.0",
		"claude-opus-4.7":          "claude-opus-4.7",
		"claude-opus-4-7-1m":       "claude-opus-4.7",
		"claude-opus-4.7-1m":       "claude-opus-4.7",
		"claude-opus-4-7-20260101": "claude-opus-4.7",

		"claude-opus-4-6-1m": "claude-opus-4.6",
		"claude-opus-4-6.1m": "claude-opus-4.6",
		"claude-opus-4.6-1m": "claude-opus-4.6",
		"claude-opus-4-6":    "claude-opus-4.6",
		"claude-opus-4.6":    "claude-opus-4.6",

		"claude-sonnet-4-6":    "claude-sonnet-4.6",
		"claude-sonnet-4.6":    "claude-sonnet-4.6",
		"claude-sonnet-4-6-1m": "claude-sonnet-4.6",
		"claude-sonnet-4.6-1m": "claude-sonnet-4.6",

		"claude-opus-4-5-20251101": "claude-opus-4.5",
		"claude-opus-4.5":          "claude-opus-4.5",
		"claude-opus-4-5":          "claude-opus-4.5",

		"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
		"claude-sonnet-4.5":          "claude-sonnet-4.5",
		"claude-sonnet-4.5-1m":       "claude-sonnet-4.5",
		"claude-sonnet-4-5":          "claude-sonnet-4.5",

		"claude-haiku-4.5":          "claude-haiku-4.5",
		"claude-haiku-4-5":          "claude-haiku-4.5",
		"claude-haiku-4-5-20251001": "claude-haiku-4.5",

		"claude-sonnet-4-7-thinking": "claude-sonnet-4.7",
	}

	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			got := c.resolveModel(input)
			if got != want {
				t.Fatalf("resolveModel(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestResolveModelLeavesOtherFallbacksUnchanged(t *testing.T) {
	c := &Client{cfg: &config.Config{ModelMap: map[string]string{}}}

	tests := []string{
		"gpt-4o",
		"claude-3-5-sonnet-20241022",
		"claude-sonnet-latest",
	}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			got := c.resolveModel(input)
			if got != input {
				t.Fatalf("resolveModel(%q) = %q, want unchanged", input, got)
			}
		})
	}
}
