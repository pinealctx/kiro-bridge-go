package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"kiro-bridge-go/counter"
	"kiro-bridge-go/sanitizer"
)

func (s *Server) handleMessages(c *gin.Context) {

	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"type": "invalid_request_error", "message": "invalid JSON"})
		return
	}

	if s.cfg.Debug {
		bodyBin, _ := json.MarshalIndent(body, "", "  ")
		log.Printf("[debug] handleMessages raw body: %s", string(bodyBin))
	}

	// Convert Anthropic messages to OpenAI format
	anthropicMessages := toMessages(body["messages"])
	system := body["system"]
	openaiMessages := anthropicMessagesToOpenAI(anthropicMessages, system)

	model, _ := body["model"].(string)
	reqModel := model != ""
	if model == "" {
		model = s.cfg.DefaultModel
	}
	stream, _ := body["stream"].(bool)

	// Convert tools
	var tools []map[string]interface{}
	if rawTools, ok := body["tools"].([]interface{}); ok {
		tools = filterValidTools(anthropicToolsToOpenAI(rawTools))
	}

	// Convert tool_choice
	toolChoice := convertToolChoice(body["tool_choice"])
	if toolChoice == "none" {
		tools = nil
	}

	log.Printf("messages: model=%s reqHasModel=%v, messages=%d tools=%d stream=%v", model, reqModel, len(openaiMessages), len(tools), stream)

	accessToken, err := s.tm.GetAccessToken(s.cfg.IdcRefreshURL)
	if err != nil {
		c.JSON(503, gin.H{"type": "service_unavailable", "message": err.Error()})
		return
	}
	profileARN := s.tm.ProfileARN()
	s.client.IsExternalIdP = s.tm.IsExternalIdP

	if stream {
		s.streamAnthropicResponse(c, accessToken, openaiMessages, model, profileARN, tools, anthropicMessages)
	} else {
		s.nonStreamAnthropicResponse(c, accessToken, openaiMessages, model, profileARN, tools, anthropicMessages)
	}
}

func (s *Server) handleCountTokens(c *gin.Context) {
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"type": "invalid_request_error", "message": "invalid JSON"})
		return
	}
	messages := toMessages(body["messages"])
	system := body["system"]
	openaiMessages := anthropicMessagesToOpenAI(messages, system)
	tokens := counter.EstimateMessagesTokensJSON(openaiMessages)
	c.JSON(200, gin.H{"input_tokens": tokens})
}

func sseEvent(eventType, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
}

func (s *Server) streamAnthropicResponse(c *gin.Context, accessToken string, messages []map[string]interface{}, model, profileARN string, tools []map[string]interface{}, origMessages []map[string]interface{}) {
	msgID := "msg_" + uuid.New().String()[:24]
	created := time.Now().Unix()
	convID := uuid.New().String()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(200)

	w := c.Writer

	// message_start
	startData, _ := json.Marshal(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": msgID, "type": "message", "role": "assistant",
			"model": model, "content": []interface{}{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
		},
	})
	fmt.Fprint(w, sseEvent("message_start", string(startData)))

	// content_block_start (text block at index 0)
	cbStart, _ := json.Marshal(map[string]interface{}{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	fmt.Fprint(w, sseEvent("content_block_start", string(cbStart)))
	w.(http.Flusher).Flush()

	var textBuf strings.Builder
	var rawRsp strings.Builder
	var cwRsp strings.Builder
	var toolCallsSeen []string
	outputTruncated := false
	contextUsagePercentage := float64(0)
	continuationCount := 0
	blockIndex := 0
	textBlockOpen := true // we already sent content_block_start for text at index 0

	type activeTool struct {
		id       string
		name     string
		inputBuf strings.Builder
	}
	var active *activeTool

	// closeTextBlock closes the current text block if open
	closeTextBlock := func() {
		if textBlockOpen {
			cbStop, _ := json.Marshal(map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
			fmt.Fprint(w, sseEvent("content_block_stop", string(cbStop)))
			blockIndex++
			textBlockOpen = false
		}
	}

	// ensureTextBlock opens a new text block if not already open
	ensureTextBlock := func() {
		if !textBlockOpen {
			cbStart, _ := json.Marshal(map[string]interface{}{
				"type": "content_block_start", "index": blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			fmt.Fprint(w, sseEvent("content_block_start", string(cbStart)))
			textBlockOpen = true
		}
	}

	doStream := func(msgs []map[string]interface{}) {

		rawRsp.Reset()
		cwRsp.Reset()

		reader, closer, err := s.client.GenerateStream(accessToken, msgs, model, profileARN, tools, convID)
		if err != nil {
			errData, _ := json.Marshal(map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": "api_error", "message": err.Error()}})
			fmt.Fprint(w, sseEvent("error", string(errData)))
			w.(http.Flusher).Flush()
			return
		}
		defer closer.Close()

		for {
			msg, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Printf("stream read error: %v", err)
				break
			}

			switch msg.EventType {
			case "assistantResponseEvent":
				content, _ := msg.Payload["content"].(string)
				if content != "" {
					if s.cfg.Debug {
						rawRsp.WriteString(content)
					}
					content = sanitizer.SanitizeText(content, true)
					if s.cfg.Debug {
						cwRsp.WriteString(content)
					}
					if content != "" {
						ensureTextBlock()
						textBuf.WriteString(content)
						deltaData, _ := json.Marshal(map[string]interface{}{
							"type": "content_block_delta", "index": blockIndex,
							"delta": map[string]interface{}{"type": "text_delta", "text": content},
						})
						fmt.Fprint(w, sseEvent("content_block_delta", string(deltaData)))
						w.(http.Flusher).Flush()
					}
				}

			case "toolUseEvent":
				name, _ := msg.Payload["name"].(string)
				toolUseID, _ := msg.Payload["toolUseId"].(string)
				isStop, _ := msg.Payload["stop"].(bool)

				if isStop {
					if active != nil && !sanitizer.KiroBuiltinTools[active.name] {
						// Close text block if open, then emit complete tool_use block
						closeTextBlock()
						cbStart2, _ := json.Marshal(map[string]interface{}{
							"type": "content_block_start", "index": blockIndex,
							"content_block": map[string]interface{}{"type": "tool_use", "id": active.id, "name": active.name, "input": map[string]interface{}{}},
						})
						fmt.Fprint(w, sseEvent("content_block_start", string(cbStart2)))

						// Parse accumulated input and send as complete JSON
						inputStr := active.inputBuf.String()
						var inputObj interface{}
						if inputStr != "" {
							if json.Unmarshal([]byte(inputStr), &inputObj) != nil {
								inputObj = map[string]interface{}{"raw": inputStr}
							}
						} else {
							inputObj = map[string]interface{}{}
						}
						inputJSON, _ := json.Marshal(inputObj)
						if s.cfg.Debug {
							prettyJson, inerr := json.MarshalIndent(inputObj, "", "  ")
							rawRsp.WriteString("name: " + active.name + "\n")
							cwRsp.WriteString("name: " + active.name + "\n")
							if inerr == nil {
								rawRsp.Write(prettyJson)
								cwRsp.Write(prettyJson)
							} else {
								rawRsp.Write(inputJSON)
								cwRsp.Write(inputJSON)
							}
						}
						deltaData, _ := json.Marshal(map[string]interface{}{
							"type": "content_block_delta", "index": blockIndex,
							"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(inputJSON)},
						})
						fmt.Fprint(w, sseEvent("content_block_delta", string(deltaData)))

						cbStop, _ := json.Marshal(map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
						fmt.Fprint(w, sseEvent("content_block_stop", string(cbStop)))
						toolCallsSeen = append(toolCallsSeen, active.name)
						blockIndex++
					}
					active = nil
					w.(http.Flusher).Flush()
					continue
				}
				if name != "" && toolUseID != "" && active == nil {
					active = &activeTool{id: toolUseID, name: name}
				}
				if inputFrag, ok := msg.Payload["input"].(string); ok && active != nil {
					active.inputBuf.WriteString(inputFrag)
				}

			case "contextUsageEvent":
				pct, _ := msg.Payload["contextUsagePercentage"].(float64)
				contextUsagePercentage = pct
				log.Printf("convID: %s, contextUsagePercentage is %v", convID, contextUsagePercentage)
				if pct > 95 {
					outputTruncated = true
				}
				if s.cfg.Debug {
					rawRsp.WriteString(fmt.Sprintf("contextUsagePercentage is %v", contextUsagePercentage))
				}

			case "exception":
				errMsg, _ := msg.Payload["message"].(string)
				errData, _ := json.Marshal(map[string]interface{}{
					"type":  "error",
					"error": map[string]interface{}{"type": "api_error", "message": errMsg},
				})

				if s.cfg.Debug {
					rawRsp.WriteString("\n")
					cwRsp.WriteString("\n")
					rawRsp.Write(errData)
					cwRsp.Write(errData)
				}

				fmt.Fprint(w, sseEvent("error", string(errData)))
				w.(http.Flusher).Flush()
			}
		}
	}

	doStream(messages)

	stopReason := "end_turn"
	if len(toolCallsSeen) > 0 {
		stopReason = "tool_use"
	} else if outputTruncated {
		stopReason = "max_tokens"
	}

	if s.cfg.Debug {
		log.Printf("raw streaming response, continuationCount: %d, outputTruncated: %v, stopReason: %s, content: %s", continuationCount, outputTruncated, stopReason, rawRsp.String())
		log.Printf("cw  streaming response, continuationCount: %d, outputTruncated: %v, stopReason: %s, content: %s", continuationCount, outputTruncated, stopReason, cwRsp.String())
	} else {
		log.Printf("finish streaming response, continuationCount: %d, outputTruncated: %v, stopReason: %s", continuationCount, outputTruncated, stopReason)
	}

	// Auto-continuation
	if outputTruncated && len(toolCallsSeen) == 0 && !shouldAutoContinue(textBuf.String(), messages) {
		outputTruncated = false
	}
	for outputTruncated && len(toolCallsSeen) == 0 && continuationCount < maxContinuations {
		continuationCount++
		outputTruncated = false
		active = nil
		contMessages := append(append([]map[string]interface{}{}, messages...),
			map[string]interface{}{"role": "assistant", "content": textBuf.String()},
			map[string]interface{}{"role": "user", "content": continuationPrompt},
		)
		log.Printf("Auto-continuation attempt %d: messages=%d", continuationCount, len(contMessages))
		doStream(contMessages)
		if outputTruncated && !shouldAutoContinue(textBuf.String(), messages) {
			outputTruncated = false
			break
		}
	}

	// Close final text block if still open
	closeTextBlock()

	// message_delta
	stopReason = "end_turn"
	if len(toolCallsSeen) > 0 {
		stopReason = "tool_use"
	} else if outputTruncated {
		stopReason = "max_tokens"
	}
	promptTokens := counter.EstimateMessagesTokensJSON(origMessages)
	completionTokens := counter.EstimateTokens(textBuf.String())
	deltaData, _ := json.Marshal(map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": completionTokens},
	})
	fmt.Fprint(w, sseEvent("message_delta", string(deltaData)))

	// message_stop
	stopData, _ := json.Marshal(map[string]interface{}{
		"type": "message_stop",
		"amazon-bedrock-invocationMetrics": map[string]interface{}{
			"inputTokenCount":   promptTokens,
			"outputTokenCount":  completionTokens,
			"invocationLatency": int(time.Since(time.Unix(created, 0)).Milliseconds()),
		},
	})
	fmt.Fprint(w, sseEvent("message_stop", string(stopData)))
	w.(http.Flusher).Flush()

	log.Printf("StreamEnd: outputTruncated: %v, stopReason: %s, inputTokenCount: %v, outputTokenCount: %v, contextUsagePercentage: %.3f", outputTruncated, stopReason, promptTokens, completionTokens, contextUsagePercentage)
}

func (s *Server) nonStreamAnthropicResponse(c *gin.Context, accessToken string, messages []map[string]interface{}, model, profileARN string, tools []map[string]interface{}, origMessages []map[string]interface{}) {
	msgID := "msg_" + uuid.New().String()[:24]
	created := time.Now().Unix()
	convID := uuid.New().String()

	var textParts []string
	var toolUses []map[string]interface{}
	outputTruncated := false
	continuationCount := 0

	type activeTool struct {
		id       string
		name     string
		inputBuf strings.Builder
	}
	var active *activeTool

	collectEvents := func(msgs []map[string]interface{}) error {
		reader, closer, err := s.client.GenerateStream(accessToken, msgs, model, profileARN, tools, convID)
		if err != nil {
			return err
		}
		defer closer.Close()

		for {
			msg, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}

			switch msg.EventType {
			case "assistantResponseEvent":
				content, _ := msg.Payload["content"].(string)
				if content != "" {
					textParts = append(textParts, content)
				}

			case "toolUseEvent":
				name, _ := msg.Payload["name"].(string)
				toolUseID, _ := msg.Payload["toolUseId"].(string)
				isStop, _ := msg.Payload["stop"].(bool)

				if isStop {
					if active != nil && !sanitizer.KiroBuiltinTools[active.name] {
						inputStr := active.inputBuf.String()
						var inputObj interface{}
						if json.Unmarshal([]byte(inputStr), &inputObj) != nil {
							inputObj = map[string]interface{}{}
						}
						toolUses = append(toolUses, map[string]interface{}{
							"type":  "tool_use",
							"id":    active.id,
							"name":  active.name,
							"input": inputObj,
						})
					}
					active = nil
					continue
				}
				if name != "" && toolUseID != "" && active == nil {
					active = &activeTool{id: toolUseID, name: name}
				}
				if inputFrag, ok := msg.Payload["input"].(string); ok && active != nil {
					active.inputBuf.WriteString(inputFrag)
				}

			case "toolUse":
				name, _ := msg.Payload["name"].(string)
				toolUseID, _ := msg.Payload["toolUseId"].(string)
				if sanitizer.KiroBuiltinTools[name] {
					continue
				}
				toolInput := msg.Payload["input"]
				if toolUseID == "" {
					toolUseID = "toolu_" + uuid.New().String()[:24]
				}
				toolUses = append(toolUses, map[string]interface{}{
					"type":  "tool_use",
					"id":    toolUseID,
					"name":  name,
					"input": toolInput,
				})

			case "contextUsageEvent":
				pct, _ := msg.Payload["contextUsagePercentage"].(float64)
				if pct > 0.95 {
					outputTruncated = true
				}

			case "exception":
				errMsg, _ := msg.Payload["message"].(string)
				return fmt.Errorf("CodeWhisperer error: %s", errMsg)
			}
		}
		return nil
	}

	if err := collectEvents(messages); err != nil {
		c.JSON(502, gin.H{"type": "api_error", "message": err.Error()})
		return
	}

	fullText := strings.Join(textParts, "")
	if outputTruncated && len(toolUses) == 0 && !shouldAutoContinue(fullText, messages) {
		outputTruncated = false
	}
	for outputTruncated && len(toolUses) == 0 && continuationCount < maxContinuations {
		continuationCount++
		outputTruncated = false
		active = nil
		contMessages := append(append([]map[string]interface{}{}, messages...),
			map[string]interface{}{"role": "assistant", "content": fullText},
			map[string]interface{}{"role": "user", "content": continuationPrompt},
		)
		if err := collectEvents(contMessages); err != nil {
			log.Printf("Continuation error: %v", err)
			break
		}
		fullText = strings.Join(textParts, "")
		if outputTruncated && !shouldAutoContinue(fullText, messages) {
			outputTruncated = false
			break
		}
	}

	fullText = sanitizer.SanitizeText(strings.Join(textParts, ""), false)

	stopReason := "end_turn"
	if len(toolUses) > 0 {
		stopReason = "tool_use"
	} else if outputTruncated {
		stopReason = "max_tokens"
	}

	// Build content blocks
	var content []interface{}
	if fullText != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": fullText})
	}
	for _, tu := range toolUses {
		content = append(content, tu)
	}
	if len(content) == 0 {
		content = []interface{}{}
	}

	promptTokens := counter.EstimateMessagesTokensJSON(origMessages)
	completionTokens := counter.EstimateTokens(fullText)

	if s.cfg.Debug {
		contentBin, _ := json.MarshalIndent(content, "", "  ")
		log.Printf("receive non-stream response: msgID: %v, stopReason: %v, content: %s", msgID, stopReason, string(contentBin))
	} else {
		log.Printf("receive non-stream response: msgID: %v, stopReason: %v, content length: %d chars", msgID, stopReason, len(fullText))
	}

	c.JSON(200, gin.H{
		"id":            msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": gin.H{
			"input_tokens":  promptTokens,
			"output_tokens": completionTokens,
		},
		"created": created,
	})
}

// anthropicMessagesToOpenAI converts Anthropic message format to OpenAI format.
func anthropicMessagesToOpenAI(messages []map[string]interface{}, system interface{}) []map[string]interface{} {
	var result []map[string]interface{}

	// System prompt
	if system != nil {
		switch v := system.(type) {
		case string:
			if v != "" {
				result = append(result, map[string]interface{}{"role": "system", "content": v})
			}
		case []interface{}:
			var parts []string
			for _, block := range v {
				if b, ok := block.(map[string]interface{}); ok {
					if b["type"] == "text" {
						if t, ok := b["text"].(string); ok {
							parts = append(parts, t)
						}
					}
				}
			}
			if len(parts) > 0 {
				result = append(result, map[string]interface{}{"role": "system", "content": strings.Join(parts, "\n")})
			}
		}
	}

	for _, msg := range messages {
		role, _ := msg["role"].(string)
		content := msg["content"]

		if role == "assistant" {
			if blocks, ok := content.([]interface{}); ok {
				var textParts []string
				var toolCalls []interface{}
				for _, block := range blocks {
					b, ok := block.(map[string]interface{})
					if !ok {
						continue
					}
					switch b["type"] {
					case "text":
						if t, ok := b["text"].(string); ok {
							textParts = append(textParts, t)
						}
					case "thinking":
						// skip
					case "tool_use":
						inputJSON, _ := json.Marshal(b["input"])
						toolCalls = append(toolCalls, map[string]interface{}{
							"id":   b["id"],
							"type": "function",
							"function": map[string]interface{}{
								"name":      b["name"],
								"arguments": string(inputJSON),
							},
						})
					}
				}
				openaiMsg := map[string]interface{}{
					"role":    "assistant",
					"content": strings.Join(textParts, "\n"),
				}
				if len(toolCalls) > 0 {
					openaiMsg["tool_calls"] = toolCalls
				}
				result = append(result, openaiMsg)
				continue
			}
		}

		if role == "user" {
			if blocks, ok := content.([]interface{}); ok {
				hasToolResults := false
				for _, block := range blocks {
					if b, ok := block.(map[string]interface{}); ok && b["type"] == "tool_result" {
						hasToolResults = true
						break
					}
				}
				if hasToolResults {
					for _, block := range blocks {
						b, ok := block.(map[string]interface{})
						if !ok {
							continue
						}
						if b["type"] == "tool_result" {
							tc := b["content"]
							var tcStr string
							switch v := tc.(type) {
							case string:
								tcStr = v
							case []interface{}:
								var parts []string
								for _, item := range v {
									if m, ok := item.(map[string]interface{}); ok && m["type"] == "text" {
										parts = append(parts, m["text"].(string))
									}
								}
								tcStr = strings.Join(parts, "\n")
							}
							result = append(result, map[string]interface{}{
								"role":         "tool",
								"tool_call_id": b["tool_use_id"],
								"content":      tcStr,
							})
						} else if b["type"] == "text" {
							result = append(result, map[string]interface{}{
								"role":    "user",
								"content": b["text"],
							})
						}
					}
					continue
				}
				// Convert image blocks
				var converted []interface{}
				for _, block := range blocks {
					b, ok := block.(map[string]interface{})
					if !ok {
						converted = append(converted, block)
						continue
					}
					if b["type"] == "image" {
						source, _ := b["source"].(map[string]interface{})
						if source["type"] == "base64" {
							mt, _ := source["media_type"].(string)
							data, _ := source["data"].(string)
							converted = append(converted, map[string]interface{}{
								"type": "image_url",
								"image_url": map[string]interface{}{
									"url": fmt.Sprintf("data:%s;base64,%s", mt, data),
								},
							})
						} else if source["type"] == "url" {
							converted = append(converted, map[string]interface{}{
								"type":      "image_url",
								"image_url": map[string]interface{}{"url": source["url"]},
							})
						} else {
							converted = append(converted, block)
						}
					} else {
						converted = append(converted, block)
					}
				}
				result = append(result, map[string]interface{}{"role": "user", "content": converted})
				continue
			}
		}

		result = append(result, msg)
	}

	return result
}

func anthropicToolsToOpenAI(tools []interface{}) []map[string]interface{} {
	var result []map[string]interface{}
	for _, t := range tools {
		tool, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		// Already OpenAI format
		if _, ok := tool["function"]; ok {
			result = append(result, tool)
			continue
		}
		// Anthropic format: {name, description, input_schema}
		name, _ := tool["name"].(string)
		desc, _ := tool["description"].(string)
		inputSchema := tool["input_schema"]
		if inputSchema == nil {
			inputSchema = map[string]interface{}{}
		}
		result = append(result, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": desc,
				"parameters":  inputSchema,
			},
		})
	}
	return result
}

func convertToolChoice(tc interface{}) string {
	if tc == nil {
		return ""
	}
	switch v := tc.(type) {
	case string:
		switch v {
		case "any":
			return "required"
		case "none":
			return "none"
		default:
			return v
		}
	case map[string]interface{}:
		tcType, _ := v["type"].(string)
		switch tcType {
		case "any":
			return "required"
		case "none":
			return "none"
		case "tool":
			name, _ := v["name"].(string)
			b, _ := json.Marshal(map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": name},
			})
			return string(b)
		}
	}
	return ""
}
