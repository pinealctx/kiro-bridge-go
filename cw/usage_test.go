package cw

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinealctx/kiro-bridge-go/config"
)

func TestGetUsageLimitsParsesCreditUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/" {
			t.Fatalf("path = %s, want /", r.URL.Path)
		}
		if got := r.URL.Query().Get("origin"); got != "KIRO_CLI" {
			t.Fatalf("origin = %q, want KIRO_CLI", got)
		}
		if got := r.URL.Query().Get("resourceType"); got != "AGENTIC_REQUEST" {
			t.Fatalf("resourceType = %q, want AGENTIC_REQUEST", got)
		}
		if got := r.URL.Query().Get("isEmailRequired"); got != "true" {
			t.Fatalf("isEmailRequired = %q, want true", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		if got := r.Header.Get("X-Amz-Target"); got != kiroUsageTarget {
			t.Fatalf("X-Amz-Target = %q, want %q", got, kiroUsageTarget)
		}
		if got := r.Header.Get("TokenType"); got != "EXTERNAL_IDP" {
			t.Fatalf("TokenType = %q, want EXTERNAL_IDP", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(body) != "{}" {
			t.Fatalf("body = %q, want {}", body)
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"daysUntilReset": 5,
			"nextDateReset": 1767225600000,
			"subscriptionInfo": {
				"subscriptionName": "builder",
				"subscriptionTitle": "Kiro Pro",
				"subscriptionType": "paid",
				"type": "PRO",
				"status": "ACTIVE"
			},
			"usageBreakdownList": [
				{
					"resourceType": "TOKEN",
					"currentUsage": 1,
					"currentUsageWithPrecision": 1,
					"usageLimit": 2,
					"usageLimitWithPrecision": 2,
					"displayName": "Token"
				},
				{
					"resourceType": "CREDIT",
					"currentUsage": 6580,
					"currentUsageWithPrecision": 6580,
					"usageLimit": 10000,
					"usageLimitWithPrecision": 10000,
					"currentOverages": 0,
					"overageRate": 0,
					"overageCap": 0,
					"currency": "USD",
					"displayName": "Credit"
				}
			]
		}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/generateAssistantResponse", &config.Config{})
	c.IsExternalIdP = true

	limits, err := c.GetUsageLimits(context.Background(), "test-token")
	if err != nil {
		t.Fatalf("GetUsageLimits() error = %v", err)
	}
	if limits.Tier != "Kiro Pro" {
		t.Fatalf("Tier = %q, want Kiro Pro", limits.Tier)
	}
	if limits.Usage.ResourceType != "CREDIT" {
		t.Fatalf("ResourceType = %q, want CREDIT", limits.Usage.ResourceType)
	}
	if limits.Usage.LimitPrecise != 10000 {
		t.Fatalf("LimitPrecise = %v, want 10000", limits.Usage.LimitPrecise)
	}
	if limits.Usage.UsedPrecise != 6580 {
		t.Fatalf("UsedPrecise = %v, want 6580", limits.Usage.UsedPrecise)
	}
	if limits.Usage.PercentUsed != 65.8 {
		t.Fatalf("PercentUsed = %v, want 65.8", limits.Usage.PercentUsed)
	}
}
