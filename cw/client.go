package cw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pinealctx/kiro-bridge-go/config"
	"github.com/pinealctx/kiro-bridge-go/thinking"
)

// Client is the CodeWhisperer HTTP client.
type Client struct {
	http          *http.Client
	cwURL         string
	IsExternalIdP bool
	cfg           *config.Config
}

// NewClient creates a new CodeWhisperer client.
func NewClient(cwURL string, cfg *config.Config) *Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   7200 * time.Second,
		},
		cwURL: cwURL,
		cfg:   cfg,
	}
}

var retryBackoff = []time.Duration{1 * time.Second, 3 * time.Second, 10 * time.Second}

// GenerateStream sends a request to CW and returns an EventStream reader.
// The caller must close the returned io.ReadCloser.
func (c *Client) GenerateStream(
	accessToken string,
	messages []map[string]interface{},
	model string,
	profileARN string,
	tools []map[string]interface{},
	conversationID string,
	thinkCfg ...thinking.Config,
) (*Reader, io.Closer, error) {
	cwModel := c.resolveModel(model)
	cwReq := OpenAIToCW(messages, cwModel, tools, profileARN, conversationID, thinkCfg...)

	body, err := json.Marshal(cwReq)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}

	headers := map[string]string{
		"Content-Type":                "application/x-amz-json-1.0",
		"Authorization":               "Bearer " + accessToken,
		"x-amz-target":                "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		"x-amzn-codewhisperer-optout": "true",
		"User-Agent":                  "kiro-cli-chat-macos-aarch64-1.27.2",
	}
	if c.IsExternalIdP {
		headers["TokenType"] = "EXTERNAL_IDP"
	}

	var lastErr error
	for attempt := 0; attempt < len(retryBackoff)+1; attempt++ {
		if attempt > 0 {
			time.Sleep(retryBackoff[attempt-1])
		}

		log.Printf("Do CW request [attempt=%d]: conversationID: %s, reqModel: %s -> cwModel: %s, tools: %d, messages: %d", attempt, conversationID, model, cwModel, len(messages), len(messages))

		req, err := http.NewRequest("POST", c.cwURL, bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("CW request failed (attempt %d): %v", attempt+1, err)
			continue
		}

		if resp.StatusCode == 200 {
			return NewReader(resp.Body), resp.Body, nil
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		errText := string(respBody)
		if len(errText) > 500 {
			errText = errText[:500]
		}

		// 4xx: don't retry
		if resp.StatusCode < 500 {
			return nil, nil, fmt.Errorf("CodeWhisperer API error: %d %s", resp.StatusCode, errText)
		}

		// 5xx: retry
		lastErr = fmt.Errorf("CodeWhisperer API error: %d %s", resp.StatusCode, errText)
		log.Printf("CW %d, retry %d/%d in %v", resp.StatusCode, attempt+1, len(retryBackoff)+1, retryBackoff[min(attempt, len(retryBackoff)-1)])
	}

	return nil, nil, fmt.Errorf("CodeWhisperer failed after %d attempts: %w", len(retryBackoff)+1, lastErr)
}

func (c *Client) resolveModel(model string) string {
	// Strip -thinking suffix before lookup
	model = strings.TrimSuffix(model, "-thinking")
	cwModel, ok := c.cfg.ModelMap[model]
	if !ok {
		return normalizeClaudeModel(model)
	}
	return cwModel
}

func normalizeClaudeModel(model string) string {
	if !strings.HasPrefix(model, "claude-") {
		return model
	}

	parts := strings.Split(model, "-")
	if len(parts) < 3 {
		return model
	}

	for i := 2; i < len(parts); i++ {
		if major, minor, ok := parseClaudeVersion(parts[i:]); ok {
			return strings.Join(parts[:i], "-") + "-" + major + "." + minor
		}
	}

	return model
}

func parseClaudeVersion(parts []string) (string, string, bool) {
	token := parts[0]
	if dot := strings.IndexByte(token, '.'); dot > 0 {
		major := token[:dot]
		minor, ok := parseClaudeMinor(token[dot+1:])
		return major, minor, ok && isShortNumber(major)
	}

	if len(parts) < 2 || !isShortNumber(token) {
		return "", "", false
	}

	minor, ok := parseClaudeMinor(parts[1])
	return token, minor, ok
}

func parseClaudeMinor(s string) (string, bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 || end > 2 {
		return "", false
	}
	if suffix := s[end:]; suffix != "" && suffix != ".1m" {
		return "", false
	}
	return s[:end], true
}

func isShortNumber(s string) bool {
	if len(s) == 0 || len(s) > 2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
