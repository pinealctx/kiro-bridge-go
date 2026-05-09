package cw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

const kiroUsageTarget = "AmazonCodeWhispererService.GetUsageLimits"

// UsageLimits holds Kiro subscription and quota information.
type UsageLimits struct {
	Tier              string     `json:"tier,omitempty"`
	SubscriptionName  string     `json:"subscription_name,omitempty"`
	SubscriptionTitle string     `json:"subscription_title,omitempty"`
	SubscriptionType  string     `json:"subscription_type,omitempty"`
	SubscriptionState string     `json:"subscription_state,omitempty"`
	DaysUntilReset    *int       `json:"days_until_reset,omitempty"`
	NextDateReset     *int64     `json:"next_date_reset,omitempty"`
	Usage             UsageQuota `json:"usage"`
}

// UsageQuota holds the current quota counters returned by Kiro.
type UsageQuota struct {
	ResourceType     string  `json:"resource_type,omitempty"`
	DisplayName      string  `json:"display_name,omitempty"`
	Used             float64 `json:"used"`
	Limit            float64 `json:"limit"`
	UsedPrecise      float64 `json:"used_precise"`
	LimitPrecise     float64 `json:"limit_precise"`
	Remaining        float64 `json:"remaining"`
	RemainingPrecise float64 `json:"remaining_precise"`
	PercentUsed      float64 `json:"percent_used"`
	OverageRate      float64 `json:"overage_rate"`
	OverageCap       float64 `json:"overage_cap"`
	Overages         float64 `json:"overages"`
	Currency         string  `json:"currency,omitempty"`
}

type usageLimitsResponse struct {
	DaysUntilReset     *int              `json:"daysUntilReset"`
	NextDateReset      *float64          `json:"nextDateReset"`
	UsageBreakdownList []usageBreakdown  `json:"usageBreakdownList"`
	SubscriptionInfo   *subscriptionInfo `json:"subscriptionInfo"`
}

type usageBreakdown struct {
	ResourceType              string  `json:"resourceType"`
	CurrentUsage              float64 `json:"currentUsage"`
	CurrentUsageWithPrecision float64 `json:"currentUsageWithPrecision"`
	UsageLimit                float64 `json:"usageLimit"`
	UsageLimitWithPrecision   float64 `json:"usageLimitWithPrecision"`
	CurrentOverages           float64 `json:"currentOverages"`
	OverageRate               float64 `json:"overageRate"`
	OverageCap                float64 `json:"overageCap"`
	Currency                  string  `json:"currency"`
	DisplayName               string  `json:"displayName"`
	DisplayNamePlural         string  `json:"displayNamePlural"`
}

type subscriptionInfo struct {
	SubscriptionName  string `json:"subscriptionName"`
	SubscriptionTitle string `json:"subscriptionTitle"`
	SubscriptionType  string `json:"subscriptionType"`
	Type              string `json:"type"`
	Status            string `json:"status"`
}

// GetUsageLimits fetches the current Kiro subscription and quota limits.
func (c *Client) GetUsageLimits(ctx context.Context, accessToken string) (*UsageLimits, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("access token is empty")
	}

	reqBody := []byte("{}")
	query := url.Values{}
	query.Set("origin", "KIRO_CLI")
	query.Set("resourceType", "AGENTIC_REQUEST")
	query.Set("isEmailRequired", "true")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.restEndpoint()+"?"+query.Encode(), bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create usage request: %w", err)
	}
	setKiroRestHeaders(req, accessToken, c.IsExternalIdP, kiroUsageTarget)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("usage request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	if err != nil {
		return nil, fmt.Errorf("read usage response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage API returned %d: %s", resp.StatusCode, truncateForError(string(body), 300))
	}

	var result usageLimitsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse usage response: %w", err)
	}

	limits := &UsageLimits{
		DaysUntilReset: result.DaysUntilReset,
	}
	if result.NextDateReset != nil {
		nextDateReset := int64(*result.NextDateReset)
		limits.NextDateReset = &nextDateReset
	}
	if result.SubscriptionInfo != nil {
		limits.SubscriptionName = result.SubscriptionInfo.SubscriptionName
		limits.SubscriptionTitle = result.SubscriptionInfo.SubscriptionTitle
		limits.SubscriptionType = result.SubscriptionInfo.SubscriptionType
		limits.SubscriptionState = result.SubscriptionInfo.Status
		limits.Tier = firstNonEmpty(result.SubscriptionInfo.SubscriptionTitle, result.SubscriptionInfo.SubscriptionName, result.SubscriptionInfo.SubscriptionType, result.SubscriptionInfo.Type)
	}
	if len(result.UsageBreakdownList) > 0 {
		breakdown := result.UsageBreakdownList[0]
		for _, b := range result.UsageBreakdownList {
			if b.ResourceType == "CREDIT" {
				breakdown = b
				break
			}
		}
		usedPrecise := firstPositive(breakdown.CurrentUsageWithPrecision, breakdown.CurrentUsage)
		limitPrecise := firstPositive(breakdown.UsageLimitWithPrecision, breakdown.UsageLimit)
		limits.Usage = UsageQuota{
			ResourceType:     breakdown.ResourceType,
			DisplayName:      firstNonEmpty(breakdown.DisplayName, breakdown.DisplayNamePlural),
			Used:             breakdown.CurrentUsage,
			Limit:            breakdown.UsageLimit,
			UsedPrecise:      usedPrecise,
			LimitPrecise:     limitPrecise,
			Remaining:        maxFloat(0, breakdown.UsageLimit-breakdown.CurrentUsage),
			RemainingPrecise: maxFloat(0, limitPrecise-usedPrecise),
			OverageRate:      breakdown.OverageRate,
			OverageCap:       breakdown.OverageCap,
			Overages:         breakdown.CurrentOverages,
			Currency:         breakdown.Currency,
		}
		if limitPrecise > 0 {
			limits.Usage.PercentUsed = usedPrecise / limitPrecise * 100
		}
	}

	return limits, nil
}

func (c *Client) restEndpoint() string {
	u, err := url.Parse(c.cwURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "https://q.us-east-1.amazonaws.com/"
	}
	u.Path = "/"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func setKiroRestHeaders(req *http.Request, accessToken string, externalIDP bool, target string) {
	apiName := "codewhispererruntime"
	ua := fmt.Sprintf("aws-sdk-rust/1.3.14 ua/2.1 api/%s/0.1.14474 os/%s lang/rust/1.92.0 md/appVersion-1.28.1 app/AmazonQ-For-CLI", apiName, runtime.GOOS)
	xAmzUA := fmt.Sprintf("aws-sdk-rust/1.3.14 ua/2.1 api/%s/0.1.14474 os/%s lang/rust/1.92.0 m/F,C app/AmazonQ-For-CLI", apiName, runtime.GOOS)

	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-Amz-Target", target)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("X-Amz-User-Agent", xAmzUA)
	req.Header.Set("X-Amzn-Codewhisperer-Optout", "false")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Accept", "*/*")
	if externalIDP {
		req.Header.Set("TokenType", "EXTERNAL_IDP")
	}
}

func truncateForError(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstPositive(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
