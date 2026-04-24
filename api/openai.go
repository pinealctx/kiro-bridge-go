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

	"github.com/pinealctx/kiro-bridge-go/counter"
	"github.com/pinealctx/kiro-bridge-go/sanitizer"
	"github.com/pinealctx/kiro-bridge-go/thinking"
)

const (
	maxContinuations         = 5
	minOutputForContinuation = 4000
)

var continuationPrompt = "Your previous response was truncated by the system at the output limit. The content is INCOMPLETE. You MUST continue outputting from EXACTLY where you left off — pick up from the very last character. Do NOT summarize, do NOT add commentary, do NOT say 'let me know if you need more'. Just output the remaining content until it is genuinely finished."

func (s *Server) handleListModels(c *gin.Context) {
	now := int(time.Now().Unix())
	var models []gin.H
	for name := range s.cfg.ModelMap {
		models = append(models, gin.H{
			"id":           name,
			"object":       "model",
			"created":      now,
			"owned_by":     "anthropic",
			"root":         "claude-opus-4.6-1m",
			"parent":       nil,
			"capabilities": gin.H{"vision": true, "function_calling": true},
		})
	}
	c.JSON(200, gin.H{"object": "list", "data": models})
}

func (s *Server) handleChatCompletions(c *gin.Context) {
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": gin.H{"message": "invalid JSON", "type": "invalid_request_error"}})
		return
	}

	messages := toMessages(body["messages"])
	if len(messages) == 0 {
		c.JSON(400, gin.H{"error": gin.H{"message": "messages is required", "type": "invalid_request_error"}})
		return
	}

	model, _ := body["model"].(string)
	if model == "" {
		model = s.cfg.DefaultModel
	}
	stream, _ := body["stream"].(bool)
	tools := filterValidTools(toToolList(body["tools"]))
	toolChoice := body["tool_choice"]
	streamOptions, _ := body["stream_options"].(map[string]interface{})

	if toolChoice == "none" {
		tools = nil
	}

	log.Printf("chat_completions: model=%s messages=%d tools=%d stream=%v", model, len(messages), len(tools), stream)

	// Parse thinking config (supports Anthropic-style "thinking" field in OpenAI request)
	thinkCfg := thinking.ParseConfig(body)
	if !thinkCfg.Enabled {
		// Also check OpenAI-style reasoning_effort
		if effort, ok := body["reasoning_effort"].(string); ok && effort != "" {
			thinkCfg = thinking.Config{Enabled: true, Type: "adaptive", Budget: thinking.DefaultBudgetTokens, Effort: strings.ToLower(effort)}
		}
	}

	accessToken, err := s.tm.GetAccessToken(s.cfg.IdcRefreshURL)
	if err != nil {
		c.JSON(503, gin.H{"error": gin.H{"message": err.Error(), "type": "service_unavailable"}})
		return
	}
	profileARN := s.tm.ProfileARN()
	s.client.IsExternalIdP = s.tm.IsExternalIdP

	if stream {
		includeUsage := false
		if streamOptions != nil {
			includeUsage, _ = streamOptions["include_usage"].(bool)
		}
		s.streamChatResponse(c, accessToken, messages, model, profileARN, tools, includeUsage, thinkCfg)
	} else {
		s.nonStreamChatResponse(c, accessToken, messages, model, profileARN, tools, thinkCfg)
	}
}

func makeChunk(chatID string, created int64, model string, delta map[string]interface{}, finishReason *string, usage map[string]interface{}) string {
	chunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	b, _ := json.Marshal(chunk)
	return "data: " + string(b) + "\n\n"
}

func (s *Server) streamChatResponse(c *gin.Context, accessToken string, messages []map[string]interface{}, model, profileARN string, tools []map[string]interface{}, includeUsage bool, thinkCfg thinking.Config) {
	chatID := "chatcmpl-" + uuid.New().String()[:24]
	created := time.Now().Unix()
	convID := uuid.New().String()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(200)

	w := c.Writer

	var streamTextBuf strings.Builder
	var toolCallsSeen []string
	toolCallIndex := 0
	outputTruncated := false
	continuationCount := 0

	var parser *thinking.Parser
	if thinkCfg.Enabled {
		parser = thinking.NewParser()
	}

	type activeTool struct {
		id       string
		name     string
		inputBuf strings.Builder
	}
	var active *activeTool

	doStream := func(msgs []map[string]interface{}) bool {
		reader, closer, err := s.client.GenerateStream(accessToken, msgs, model, profileARN, tools, convID, thinkCfg)
		if err != nil {
			fmt.Fprintf(w, "data: {\"error\":\"%s\"}\n\n", err.Error())
			return false
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
					content = sanitizer.SanitizeText(content, true)
					if content != "" {
						if parser != nil {
							segments := parser.Push(content)
							for _, seg := range segments {
								if seg.Type == thinking.SegmentThinking {
									chunk := makeChunk(chatID, created, model, map[string]interface{}{"reasoning_content": seg.Text}, nil, nil)
									fmt.Fprint(w, chunk)
								} else {
									streamTextBuf.WriteString(seg.Text)
									chunk := makeChunk(chatID, created, model, map[string]interface{}{"content": seg.Text}, nil, nil)
									fmt.Fprint(w, chunk)
								}
							}
						} else {
							streamTextBuf.WriteString(content)
							chunk := makeChunk(chatID, created, model, map[string]interface{}{"content": content}, nil, nil)
							fmt.Fprint(w, chunk)
						}
						w.(http.Flusher).Flush()
					}
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
							inputObj = map[string]interface{}{"raw": inputStr}
						}
						arguments, _ := json.Marshal(inputObj)
						argStr := string(arguments)

						// Emit name chunk
						tcBase := map[string]interface{}{
							"index": toolCallIndex, "id": active.id, "type": "function",
							"function": map[string]interface{}{"name": active.name, "arguments": ""},
						}
						fmt.Fprint(w, makeChunk(chatID, created, model, map[string]interface{}{"tool_calls": []interface{}{tcBase}}, nil, nil))
						// Emit arguments in 40-char chunks
						for i := 0; i < len(argStr); i += 40 {
							end := i + 40
							if end > len(argStr) {
								end = len(argStr)
							}
							tcArgs := map[string]interface{}{
								"index":    toolCallIndex,
								"function": map[string]interface{}{"arguments": argStr[i:end]},
							}
							fmt.Fprint(w, makeChunk(chatID, created, model, map[string]interface{}{"tool_calls": []interface{}{tcArgs}}, nil, nil))
						}
						toolCallsSeen = append(toolCallsSeen, active.name)
						toolCallIndex++
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

			case "toolUse":
				name, _ := msg.Payload["name"].(string)
				toolUseID, _ := msg.Payload["toolUseId"].(string)
				if sanitizer.KiroBuiltinTools[name] {
					continue
				}
				toolInput := msg.Payload["input"]
				arguments, _ := json.Marshal(toolInput)
				argStr := string(arguments)
				if toolUseID == "" {
					toolUseID = "call_" + uuid.New().String()[:24]
				}
				tcBase := map[string]interface{}{
					"index": toolCallIndex, "id": toolUseID, "type": "function",
					"function": map[string]interface{}{"name": name, "arguments": ""},
				}
				fmt.Fprint(w, makeChunk(chatID, created, model, map[string]interface{}{"tool_calls": []interface{}{tcBase}}, nil, nil))
				for i := 0; i < len(argStr); i += 40 {
					end := i + 40
					if end > len(argStr) {
						end = len(argStr)
					}
					tcArgs := map[string]interface{}{
						"index":    toolCallIndex,
						"function": map[string]interface{}{"arguments": argStr[i:end]},
					}
					fmt.Fprint(w, makeChunk(chatID, created, model, map[string]interface{}{"tool_calls": []interface{}{tcArgs}}, nil, nil))
				}
				toolCallsSeen = append(toolCallsSeen, name)
				toolCallIndex++
				w.(http.Flusher).Flush()

			case "contextUsageEvent":
				pct, _ := msg.Payload["contextUsagePercentage"].(float64)
				if pct > 0.95 {
					outputTruncated = true
				}

			case "exception":
				errMsg, _ := msg.Payload["message"].(string)
				if errMsg == "" {
					b, _ := json.Marshal(msg.Payload)
					errMsg = string(b)
				}
				fr := "stop"
				fmt.Fprint(w, makeChunk(chatID, created, model, map[string]interface{}{"content": "\n\n[Error: " + errMsg + "]"}, &fr, nil))
				w.(http.Flusher).Flush()
			}
		}
		// Flush parser at stream end
		if parser != nil {
			segments := parser.Flush()
			for _, seg := range segments {
				if seg.Type == thinking.SegmentThinking {
					chunk := makeChunk(chatID, created, model, map[string]interface{}{"reasoning_content": seg.Text}, nil, nil)
					fmt.Fprint(w, chunk)
				} else {
					streamTextBuf.WriteString(seg.Text)
					chunk := makeChunk(chatID, created, model, map[string]interface{}{"content": seg.Text}, nil, nil)
					fmt.Fprint(w, chunk)
				}
			}
			w.(http.Flusher).Flush()
		}
		return true
	}

	doStream(messages)

	// Auto-continuation
	if outputTruncated && len(toolCallsSeen) == 0 && !shouldAutoContinue(streamTextBuf.String(), messages) {
		log.Printf("Auto-continuation skipped (false positive): %d chars output", streamTextBuf.Len())
		outputTruncated = false
	}
	for outputTruncated && len(toolCallsSeen) == 0 && continuationCount < maxContinuations {
		continuationCount++
		log.Printf("Auto-continuing (%d/%d), accumulated %d chars", continuationCount, maxContinuations, streamTextBuf.Len())
		outputTruncated = false
		active = nil

		contMessages := append(append([]map[string]interface{}{}, messages...),
			map[string]interface{}{"role": "assistant", "content": streamTextBuf.String()},
			map[string]interface{}{"role": "user", "content": continuationPrompt},
		)
		doStream(contMessages)

		if outputTruncated && !shouldAutoContinue(streamTextBuf.String(), messages) {
			log.Printf("Auto-continuation skipped after round %d (false positive)", continuationCount)
			outputTruncated = false
			break
		}
	}

	// Final finish chunk
	finishReason := "stop"
	if len(toolCallsSeen) > 0 {
		finishReason = "tool_calls"
	} else if outputTruncated {
		finishReason = "length"
	}
	fmt.Fprint(w, makeChunk(chatID, created, model, map[string]interface{}{}, &finishReason, nil))

	if includeUsage {
		promptTokens := counter.EstimateMessagesTokensJSON(messages)
		completionTokens := counter.EstimateTokens(streamTextBuf.String())
		usageChunk := makeChunk(chatID, created, model, map[string]interface{}{}, nil, map[string]interface{}{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		})
		fmt.Fprint(w, usageChunk)
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	w.(http.Flusher).Flush()
}

func (s *Server) nonStreamChatResponse(c *gin.Context, accessToken string, messages []map[string]interface{}, model, profileARN string, tools []map[string]interface{}, thinkCfg thinking.Config) {
	chatID := "chatcmpl-" + uuid.New().String()[:24]
	created := time.Now().Unix()
	convID := uuid.New().String()

	var textParts []string
	var toolCalls []map[string]interface{}
	toolCallIndex := 0
	outputTruncated := false
	continuationCount := 0

	type activeTool struct {
		id       string
		name     string
		inputBuf strings.Builder
	}
	var active *activeTool

	collectEvents := func(msgs []map[string]interface{}) error {
		reader, closer, err := s.client.GenerateStream(accessToken, msgs, model, profileARN, tools, convID, thinkCfg)
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
							inputObj = map[string]interface{}{"raw": inputStr}
						}
						arguments, _ := json.Marshal(inputObj)
						toolCalls = append(toolCalls, map[string]interface{}{
							"index": toolCallIndex, "id": active.id, "type": "function",
							"function": map[string]interface{}{"name": active.name, "arguments": string(arguments)},
						})
						toolCallIndex++
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
				arguments, _ := json.Marshal(toolInput)
				if toolUseID == "" {
					toolUseID = "call_" + uuid.New().String()[:24]
				}
				toolCalls = append(toolCalls, map[string]interface{}{
					"index": toolCallIndex, "id": toolUseID, "type": "function",
					"function": map[string]interface{}{"name": name, "arguments": string(arguments)},
				})
				toolCallIndex++

			case "contextUsageEvent":
				pct, _ := msg.Payload["contextUsagePercentage"].(float64)
				if pct > 0.95 {
					outputTruncated = true
				}

			case "exception":
				errMsg, _ := msg.Payload["message"].(string)
				if errMsg == "" {
					b, _ := json.Marshal(msg.Payload)
					errMsg = string(b)
				}
				return fmt.Errorf("CodeWhisperer error: %s", errMsg)
			}
		}
		return nil
	}

	if err := collectEvents(messages); err != nil {
		c.JSON(502, gin.H{"error": gin.H{"message": err.Error(), "type": "upstream_error"}})
		return
	}

	// Auto-continuation
	fullText := strings.Join(textParts, "")
	if outputTruncated && len(toolCalls) == 0 && !shouldAutoContinue(fullText, messages) {
		log.Printf("Non-stream auto-continuation skipped (false positive): %d chars output", len(fullText))
		outputTruncated = false
	}
	for outputTruncated && len(toolCalls) == 0 && continuationCount < maxContinuations {
		continuationCount++
		log.Printf("Non-stream auto-continuing (%d/%d), accumulated %d chars", continuationCount, maxContinuations, len(fullText))
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

	// Parse thinking from collected text
	var reasoningContent string
	if thinkCfg.Enabled && fullText != "" {
		p := thinking.NewParser()
		segments := p.Push(fullText)
		segments = append(segments, p.Flush()...)
		var thinkParts, textPartsParsed []string
		for _, seg := range segments {
			if seg.Type == thinking.SegmentThinking {
				thinkParts = append(thinkParts, seg.Text)
			} else {
				textPartsParsed = append(textPartsParsed, seg.Text)
			}
		}
		reasoningContent = strings.Join(thinkParts, "")
		fullText = strings.Join(textPartsParsed, "")
	}

	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	} else if outputTruncated {
		finishReason = "length"
	}

	var msgContent interface{} = fullText
	if fullText == "" {
		msgContent = nil
	}
	message := map[string]interface{}{
		"role":    "assistant",
		"content": msgContent,
	}
	if reasoningContent != "" {
		message["reasoning_content"] = reasoningContent
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	promptTokens := counter.EstimateMessagesTokensJSON(messages)
	completionTokens := counter.EstimateTokens(fullText) + counter.EstimateTokens(reasoningContent)
	for _, tc := range toolCalls {
		if fn, ok := tc["function"].(map[string]interface{}); ok {
			completionTokens += counter.EstimateTokens(fmt.Sprintf("%v", fn["name"]))
			completionTokens += counter.EstimateTokens(fmt.Sprintf("%v", fn["arguments"]))
		}
	}

	c.JSON(200, gin.H{
		"id":      chatID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage": gin.H{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	})
}

// shouldAutoContinue uses a 3-layer heuristic to detect genuine truncation.
func shouldAutoContinue(outputText string, inputMessages []map[string]interface{}) bool {
	// Layer 1: minimum output length
	if len(outputText) < minOutputForContinuation {
		return false
	}
	stripped := strings.TrimRight(outputText, " \t\n\r")
	if stripped == "" {
		return false
	}

	// Layer 2: input/output ratio
	if len(inputMessages) > 0 {
		inputTokens := counter.EstimateMessagesTokensJSON(inputMessages)
		outputTokens := counter.EstimateTokens(outputText)
		if inputTokens > 0 && outputTokens > 0 && inputTokens > outputTokens*8 {
			return false
		}
	}

	// Layer 3: ending structure
	lastChar := rune(stripped[len(stripped)-1])
	if strings.ContainsRune(".。!！?？…)）]】」》\"'", lastChar) {
		return false
	}
	lastLine := stripped
	if idx := strings.LastIndex(stripped, "\n"); idx >= 0 {
		lastLine = strings.TrimRight(stripped[idx+1:], " \t")
	}
	if lastLine == "```" || lastLine == "---" || lastLine == "***" || lastLine == "===" {
		return false
	}
	if strings.HasSuffix(strings.TrimRight(outputText, " \t"), "\n\n") {
		return false
	}

	return true
}

// Helper functions

func toMessages(v interface{}) []map[string]interface{} {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	var result []map[string]interface{}
	for _, item := range arr {
		if m, ok := item.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result
}

func toToolList(v interface{}) []map[string]interface{} {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	var result []map[string]interface{}
	for _, item := range arr {
		if m, ok := item.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result
}

func filterValidTools(tools []map[string]interface{}) []map[string]interface{} {
	var result []map[string]interface{}
	for _, t := range tools {
		if isValidTool(t) {
			result = append(result, t)
		}
	}
	return result
}

func isValidTool(tool map[string]interface{}) bool {
	if fn, ok := tool["function"].(map[string]interface{}); ok {
		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		return name != "" && desc != ""
	}
	name, _ := tool["name"].(string)
	desc, _ := tool["description"].(string)
	return name != "" && desc != ""
}
