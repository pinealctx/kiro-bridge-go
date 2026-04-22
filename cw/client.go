package cw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/pinealctx/kiro-bridge-go/config"
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
) (*Reader, io.Closer, error) {
	cwModel := c.resolveModel(model)
	cwReq := OpenAIToCW(messages, cwModel, tools, profileARN, conversationID)

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
	cwModel, ok := c.cfg.ModelMap[model]
	if !ok {
		return model
	}
	return cwModel
}
