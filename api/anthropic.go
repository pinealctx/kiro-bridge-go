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

func (s *Server) handleMessages(c *gin.Context) {

	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"type": "invalid_request_error", "message": "invalid JSON"})
		return
	}

	var bodyBin []byte
	if s.cfg.Debug {
		logBody := make(map[string]interface{}, len(body))
		for k, v := range body {
			if k == "system" || k == "tools" {
				logBody[k] = fmt.Sprintf("(%T, omitted)", v)
			} else {
				logBody[k] = v
			}
		}
		bodyBin, _ = json.MarshalIndent(logBody, "", "  ")
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

	if s.cfg.Debug {
		log.Printf("Claude code Request, model=%s reqHasModel=%v, raw body: %s", model, reqModel, string(bodyBin))
	} else {
		log.Printf("Claude code Request, model=%s reqHasModel=%v", model, reqModel)
	}

	// Parse thinking config
	thinkCfg := thinking.ParseConfig(body)
	if thinkCfg.Enabled {
		log.Printf("\033[33mthinking enabled: type=%s budget=%d effort=%s\033[0m", thinkCfg.Type, thinkCfg.Budget, thinkCfg.Effort)
	}

	accessToken, err := s.tm.GetAccessToken(s.cfg.IdcRefreshURL)
	if err != nil {
		c.JSON(503, gin.H{"type": "service_unavailable", "message": err.Error()})
		return
	}
	profileARN := s.tm.ProfileARN()
	s.client.IsExternalIdP = s.tm.IsExternalIdP

	if stream {
		s.streamAnthropicResponse(c, accessToken, openaiMessages, model, profileARN, tools, anthropicMessages, thinkCfg)
	} else {
		s.nonStreamAnthropicResponse(c, accessToken, openaiMessages, model, profileARN, tools, anthropicMessages, thinkCfg)
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

func (s *Server) streamAnthropicResponse(c *gin.Context, accessToken string, messages []map[string]interface{}, model, profileARN string, tools []map[string]interface{}, origMessages []map[string]interface{}, thinkCfg thinking.Config) {
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
	w.(http.Flusher).Flush()

	var textBuf strings.Builder
	var thinkBuf strings.Builder
	var rawRsp strings.Builder
	var cwRsp strings.Builder
	var toolCallsSeen []string
	var filteredBuiltinTools []string
	var remappedBuiltinTools []string
	clientToolNames := sanitizer.ClientToolNameSet(tools)
	outputTruncated := false
	contextUsagePercentage := float64(0)
	continuationCount := 0
	blockIndex := 0

	// Thinking state
	var parser *thinking.Parser
	if thinkCfg.Enabled {
		parser = thinking.NewParser()
	}
	thinkingBlockOpen := false
	textBlockOpen := false
	emittedMeaningfulText := false

	type activeTool struct {
		id       string
		name     string
		inputBuf strings.Builder
	}
	var active *activeTool

	closeThinkingBlock := func() {
		if thinkingBlockOpen {
			cbStop, _ := json.Marshal(map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
			fmt.Fprint(w, sseEvent("content_block_stop", string(cbStop)))
			blockIndex++
			thinkingBlockOpen = false
		}
	}

	closeTextBlock := func() {
		if textBlockOpen {
			cbStop, _ := json.Marshal(map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
			fmt.Fprint(w, sseEvent("content_block_stop", string(cbStop)))
			blockIndex++
			textBlockOpen = false
		}
	}

	ensureThinkingBlock := func() {
		if !thinkingBlockOpen {
			cbStart, _ := json.Marshal(map[string]interface{}{
				"type": "content_block_start", "index": blockIndex,
				"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
			})
			fmt.Fprint(w, sseEvent("content_block_start", string(cbStart)))
			thinkingBlockOpen = true
		}
	}

	ensureTextBlock := func() {
		closeThinkingBlock()
		if !textBlockOpen {
			cbStart, _ := json.Marshal(map[string]interface{}{
				"type": "content_block_start", "index": blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			fmt.Fprint(w, sseEvent("content_block_start", string(cbStart)))
			textBlockOpen = true
		}
	}

	emitThinkingDelta := func(text string) {
		ensureThinkingBlock()
		thinkBuf.WriteString(text)
		deltaData, _ := json.Marshal(map[string]interface{}{
			"type": "content_block_delta", "index": blockIndex,
			"delta": map[string]interface{}{"type": "thinking_delta", "thinking": text},
		})
		fmt.Fprint(w, sseEvent("content_block_delta", string(deltaData)))
		w.(http.Flusher).Flush()
	}

	emitTextDelta := func(text string) {
		ensureTextBlock()
		textBuf.WriteString(text)
		if strings.TrimSpace(text) != "" {
			emittedMeaningfulText = true
		}
		deltaData, _ := json.Marshal(map[string]interface{}{
			"type": "content_block_delta", "index": blockIndex,
			"delta": map[string]interface{}{"type": "text_delta", "text": text},
		})
		fmt.Fprint(w, sseEvent("content_block_delta", string(deltaData)))
		w.(http.Flusher).Flush()
	}

	// If thinking is not enabled, open text block immediately (original behavior)
	if !thinkCfg.Enabled {
		cbStart, _ := json.Marshal(map[string]interface{}{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		fmt.Fprint(w, sseEvent("content_block_start", string(cbStart)))
		textBlockOpen = true
	}

	totalEventCount := 0

	doStream := func(msgs []map[string]interface{}) {

		rawRsp.Reset()
		cwRsp.Reset()

		reader, closer, err := s.client.GenerateStream(accessToken, msgs, model, profileARN, tools, convID, thinkCfg)
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
			totalEventCount++

			if s.cfg.Debug {
				switch msg.EventType {
				case "assistantResponseEvent":
				case "contextUsageEvent":
				default:
					payloadBin, _ := json.Marshal(msg.Payload)
					log.Printf("[event] #%d type=%s payload=%s", totalEventCount, msg.EventType, string(payloadBin))
				}
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
						if parser != nil {
							segments := parser.Push(content)
							for _, seg := range segments {
								if seg.Type == thinking.SegmentThinking {
									emitThinkingDelta(seg.Text)
								} else {
									emitTextDelta(seg.Text)
								}
							}
						} else {
							emitTextDelta(content)
						}
					}
				}

			case "toolUseEvent":
				name, _ := msg.Payload["name"].(string)
				toolUseID, _ := msg.Payload["toolUseId"].(string)
				isStop, _ := msg.Payload["stop"].(bool)

				if isStop {
					if active != nil {
						emitToolCall := !sanitizer.KiroBuiltinTools[active.name]

						// Try remapping Kiro builtin tools to client tools
						if sanitizer.KiroBuiltinTools[active.name] {
							inputStr := active.inputBuf.String()
							var kiroInput interface{}
							if inputStr != "" {
								json.Unmarshal([]byte(inputStr), &kiroInput)
							}
							if remappedName, remappedInput, ok := sanitizer.RemapBuiltinTool(active.name, kiroInput, clientToolNames); ok {
								log.Printf("\033[36m[remap] builtin tool %s → %s (id=%s)\033[0m", active.name, remappedName, active.id)
								active.name = remappedName
								active.inputBuf.Reset()
								remappedJSON, _ := json.Marshal(remappedInput)
								active.inputBuf.Write(remappedJSON)
								remappedBuiltinTools = append(remappedBuiltinTools, remappedName)
								emitToolCall = true
							} else {
								log.Printf("\033[33m[filter] builtin tool %s dropped (no client tool match, id=%s)\033[0m", active.name, active.id)
								filteredBuiltinTools = append(filteredBuiltinTools, active.name)
							}
						}

						if emitToolCall {
							closeThinkingBlock()
							closeTextBlock()
							cbStart2, _ := json.Marshal(map[string]interface{}{
								"type": "content_block_start", "index": blockIndex,
								"content_block": map[string]interface{}{"type": "tool_use", "id": active.id, "name": active.name, "input": map[string]interface{}{}},
							})
							fmt.Fprint(w, sseEvent("content_block_start", string(cbStart2)))

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

		// Flush active tool if stream ended without a stop event
		if active != nil {
			log.Printf("\033[33m[flush-active] stream ended with pending tool: %s (id=%s), emitting\033[0m", active.name, active.id)
			emitToolCall := !sanitizer.KiroBuiltinTools[active.name]

			if sanitizer.KiroBuiltinTools[active.name] {
				inputStr := active.inputBuf.String()
				var kiroInput interface{}
				if inputStr != "" {
					json.Unmarshal([]byte(inputStr), &kiroInput)
				}
				if remappedName, remappedInput, ok := sanitizer.RemapBuiltinTool(active.name, kiroInput, clientToolNames); ok {
					log.Printf("\033[36m[remap] builtin tool %s → %s (id=%s)\033[0m", active.name, remappedName, active.id)
					active.name = remappedName
					active.inputBuf.Reset()
					remappedJSON, _ := json.Marshal(remappedInput)
					active.inputBuf.Write(remappedJSON)
					remappedBuiltinTools = append(remappedBuiltinTools, remappedName)
					emitToolCall = true
				} else {
					log.Printf("\033[33m[filter] builtin tool %s dropped (no client tool match, id=%s)\033[0m", active.name, active.id)
					filteredBuiltinTools = append(filteredBuiltinTools, active.name)
				}
			}

			if emitToolCall {
				closeThinkingBlock()
				closeTextBlock()
				cbStart2, _ := json.Marshal(map[string]interface{}{
					"type": "content_block_start", "index": blockIndex,
					"content_block": map[string]interface{}{"type": "tool_use", "id": active.id, "name": active.name, "input": map[string]interface{}{}},
				})
				fmt.Fprint(w, sseEvent("content_block_start", string(cbStart2)))

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
				deltaData, _ := json.Marshal(map[string]interface{}{
					"type": "content_block_delta", "index": blockIndex,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(inputJSON)},
				})
				fmt.Fprint(w, sseEvent("content_block_delta", string(deltaData)))

				cbStop, _ := json.Marshal(map[string]interface{}{"type": "content_block_stop", "index": blockIndex})
				fmt.Fprint(w, sseEvent("content_block_stop", string(cbStop)))
				toolCallsSeen = append(toolCallsSeen, active.name)
				blockIndex++
				w.(http.Flusher).Flush()
			}
			active = nil
		}

		// Flush parser at stream end
		if parser != nil {
			segments := parser.Flush()
			for _, seg := range segments {
				if seg.Type == thinking.SegmentThinking {
					emitThinkingDelta(seg.Text)
				} else {
					emitTextDelta(seg.Text)
				}
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
		log.Printf("raw streaming response, convID: %s, continuationCount: %d, outputTruncated: %v, stopReason: %s, content: %s", convID, continuationCount, outputTruncated, stopReason, rawRsp.String())
		//log.Printf("cw  streaming response, convID: %s, continuationCount: %d, outputTruncated: %v, stopReason: %s, content: %s", convID, continuationCount, outputTruncated, stopReason, cwRsp.String())
	} else {
		log.Printf("finish streaming response, convID: %s, continuationCount: %d, outputTruncated: %v, stopReason: %s", convID, continuationCount, outputTruncated, stopReason)
	}
	if len(remappedBuiltinTools) > 0 || len(filteredBuiltinTools) > 0 {
		log.Printf("\033[36m[remap-summary] remapped=%v filtered=%v clientTools=%d\033[0m", remappedBuiltinTools, filteredBuiltinTools, len(clientToolNames))
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

	// Handle thinking-only response: add placeholder text block
	if thinkCfg.Enabled && thinkBuf.Len() > 0 && !emittedMeaningfulText && len(toolCallsSeen) == 0 {
		closeThinkingBlock()
		ensureTextBlock()
		deltaData, _ := json.Marshal(map[string]interface{}{
			"type": "content_block_delta", "index": blockIndex,
			"delta": map[string]interface{}{"type": "text_delta", "text": " "},
		})
		fmt.Fprint(w, sseEvent("content_block_delta", string(deltaData)))
		stopReason = "max_tokens"
	}

	// Close final blocks
	closeThinkingBlock()
	closeTextBlock()

	// message_delta
	stopReason2 := "end_turn"
	if len(toolCallsSeen) > 0 {
		stopReason2 = "tool_use"
	} else if outputTruncated {
		stopReason2 = "max_tokens"
	}
	if thinkCfg.Enabled && thinkBuf.Len() > 0 && !emittedMeaningfulText && len(toolCallsSeen) == 0 {
		stopReason2 = "max_tokens"
	}
	promptTokens := counter.EstimateMessagesTokensJSON(origMessages)
	completionTokens := counter.EstimateTokens(textBuf.String()) + counter.EstimateTokens(thinkBuf.String())
	deltaData, _ := json.Marshal(map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason2, "stop_sequence": nil},
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

	log.Printf("StreamEnd: convID: %s, outputTruncated: %v, stopReason: %s, inputTokenCount: %v, outputTokenCount: %v, contextUsagePercentage: %.3f, events: %d", convID, outputTruncated, stopReason2, promptTokens, completionTokens, contextUsagePercentage, totalEventCount)
}

func (s *Server) nonStreamAnthropicResponse(c *gin.Context, accessToken string, messages []map[string]interface{}, model, profileARN string, tools []map[string]interface{}, origMessages []map[string]interface{}, thinkCfg thinking.Config) {
	msgID := "msg_" + uuid.New().String()[:24]
	created := time.Now().Unix()
	convID := uuid.New().String()

	var textParts []string
	var toolUses []map[string]interface{}
	var filteredBuiltinTools []string
	var remappedBuiltinTools []string
	clientToolNames := sanitizer.ClientToolNameSet(tools)
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
					if active != nil {
						shouldEmit := !sanitizer.KiroBuiltinTools[active.name]

						if sanitizer.KiroBuiltinTools[active.name] {
							inputStr := active.inputBuf.String()
							var kiroInput interface{}
							if inputStr != "" {
								json.Unmarshal([]byte(inputStr), &kiroInput)
							}
							if remappedName, remappedInput, ok := sanitizer.RemapBuiltinTool(active.name, kiroInput, clientToolNames); ok {
								log.Printf("\033[36m[remap] builtin tool %s → %s (id=%s)\033[0m", active.name, remappedName, active.id)
								active.name = remappedName
								active.inputBuf.Reset()
								remappedJSON, _ := json.Marshal(remappedInput)
								active.inputBuf.Write(remappedJSON)
								remappedBuiltinTools = append(remappedBuiltinTools, remappedName)
								shouldEmit = true
							} else {
								log.Printf("\033[33m[filter] builtin tool %s dropped (no client tool match, id=%s)\033[0m", active.name, active.id)
								filteredBuiltinTools = append(filteredBuiltinTools, active.name)
							}
						}

						if shouldEmit {
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
					toolInput := msg.Payload["input"]
					if remappedName, remappedInput, ok := sanitizer.RemapBuiltinTool(name, toolInput, clientToolNames); ok {
						log.Printf("\033[36m[remap] builtin toolUse %s → %s (id=%s)\033[0m", name, remappedName, toolUseID)
						remappedBuiltinTools = append(remappedBuiltinTools, remappedName)
						if toolUseID == "" {
							toolUseID = "toolu_" + uuid.New().String()[:24]
						}
						toolUses = append(toolUses, map[string]interface{}{
							"type":  "tool_use",
							"id":    toolUseID,
							"name":  remappedName,
							"input": remappedInput,
						})
					} else {
						log.Printf("\033[33m[filter] builtin toolUse %s dropped (no client tool match, id=%s)\033[0m", name, toolUseID)
						filteredBuiltinTools = append(filteredBuiltinTools, name)
					}
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
				if pct > 95 {
					outputTruncated = true
				}

			case "exception":
				errMsg, _ := msg.Payload["message"].(string)
				return fmt.Errorf("CodeWhisperer error: %s", errMsg)
			}
		}

		// Flush active tool if stream ended without a stop event
		if active != nil {
			log.Printf("\033[33m[flush-active] collectEvents ended with pending tool: %s (id=%s)\033[0m", active.name, active.id)
			shouldEmit := !sanitizer.KiroBuiltinTools[active.name]

			if sanitizer.KiroBuiltinTools[active.name] {
				inputStr := active.inputBuf.String()
				var kiroInput interface{}
				if inputStr != "" {
					json.Unmarshal([]byte(inputStr), &kiroInput)
				}
				if remappedName, remappedInput, ok := sanitizer.RemapBuiltinTool(active.name, kiroInput, clientToolNames); ok {
					log.Printf("\033[36m[remap] builtin tool %s → %s (id=%s)\033[0m", active.name, remappedName, active.id)
					active.name = remappedName
					active.inputBuf.Reset()
					remappedJSON, _ := json.Marshal(remappedInput)
					active.inputBuf.Write(remappedJSON)
					remappedBuiltinTools = append(remappedBuiltinTools, remappedName)
					shouldEmit = true
				} else {
					log.Printf("\033[33m[filter] builtin tool %s dropped (no client tool match, id=%s)\033[0m", active.name, active.id)
					filteredBuiltinTools = append(filteredBuiltinTools, active.name)
				}
			}

			if shouldEmit {
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

	// Parse thinking from collected text
	var thinkingText string
	if thinkCfg.Enabled && fullText != "" {
		parser := thinking.NewParser()
		segments := parser.Push(fullText)
		segments = append(segments, parser.Flush()...)
		var thinkParts, textPartsParsed []string
		for _, seg := range segments {
			if seg.Type == thinking.SegmentThinking {
				thinkParts = append(thinkParts, seg.Text)
			} else {
				textPartsParsed = append(textPartsParsed, seg.Text)
			}
		}
		thinkingText = strings.Join(thinkParts, "")
		fullText = strings.Join(textPartsParsed, "")
	}

	stopReason := "end_turn"
	if len(toolUses) > 0 {
		stopReason = "tool_use"
	} else if outputTruncated {
		stopReason = "max_tokens"
	}

	// Build content blocks
	var content []interface{}
	if thinkingText != "" {
		content = append(content, map[string]interface{}{"type": "thinking", "thinking": thinkingText})
	}
	if fullText != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": fullText})
	} else if thinkingText != "" && len(toolUses) == 0 {
		// Thinking-only response: add placeholder text block
		content = append(content, map[string]interface{}{"type": "text", "text": " "})
		stopReason = "max_tokens"
	}
	for _, tu := range toolUses {
		content = append(content, tu)
	}
	if len(content) == 0 {
		content = []interface{}{}
	}

	promptTokens := counter.EstimateMessagesTokensJSON(origMessages)
	completionTokens := counter.EstimateTokens(fullText) + counter.EstimateTokens(thinkingText)

	if s.cfg.Debug {
		contentBin, _ := json.MarshalIndent(content, "", "  ")
		log.Printf("receive non-stream response: msgID: %v, stopReason: %v, content: %s", msgID, stopReason, string(contentBin))
	} else {
		log.Printf("receive non-stream response: msgID: %v, stopReason: %v, content length: %d chars", msgID, stopReason, len(fullText))
	}
	if len(remappedBuiltinTools) > 0 || len(filteredBuiltinTools) > 0 {
		log.Printf("\033[36m[remap-summary] remapped=%v filtered=%v clientTools=%d\033[0m", remappedBuiltinTools, filteredBuiltinTools, len(clientToolNames))
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
				var thinkingParts []string
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
						if t, ok := b["thinking"].(string); ok {
							thinkingParts = append(thinkingParts, t)
						}
					case "redacted_thinking":
						// skip redacted thinking
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
				// Wrap thinking in tags for CW history preservation
				var combinedText string
				if len(thinkingParts) > 0 {
					combinedText = "<thinking>" + strings.Join(thinkingParts, "") + "</thinking>\n" + strings.Join(textParts, "\n")
				} else {
					combinedText = strings.Join(textParts, "\n")
				}
				openaiMsg := map[string]interface{}{
					"role":    "assistant",
					"content": combinedText,
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
