package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/gemini"
	"github.com/sonesh-2202/gemini-sentinel/internal/prompt"
	"github.com/sonesh-2202/gemini-sentinel/internal/toolbridge"
	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
)

// NormalizeResponsesInput converts an OpenAI Responses API body (`instructions`,
// `input`, `tools`, `tool_choice`) into canonical Chat Completions messages and tools.
func NormalizeResponsesInput(body map[string]any) (messages []map[string]any, tools any, toolChoice any, err error) {
	if body == nil {
		return nil, nil, nil, transport.NewUpstreamError("request body must be a JSON object", 400, "invalid_request_error", nil, nil)
	}

	if instr, ok := body["instructions"].(string); ok && strings.TrimSpace(instr) != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instr,
		})
	}

	// Support callers that send `messages` to /v1/responses as well as `input`.
	inputRaw, hasInput := body["input"]
	if !hasInput || inputRaw == nil {
		inputRaw = body["messages"]
	}

	if singleMap, ok := inputRaw.(map[string]any); ok && singleMap != nil {
		inputRaw = []any{singleMap}
	}

	switch inp := inputRaw.(type) {
	case string:
		if strings.TrimSpace(inp) != "" {
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": inp,
			})
		}
	case []any:
		var (
			pendingToolCalls []any
			pendingReasoning []string
			callIDToName     = make(map[string]string)
		)
		flushPendingAssistant := func() {
			if len(pendingToolCalls) == 0 {
				return
			}
			tcCopy := make([]any, len(pendingToolCalls))
			copy(tcCopy, pendingToolCalls)
			pendingToolCalls = nil
			msg := map[string]any{
				"role":       "assistant",
				"content":    nil,
				"tool_calls": tcCopy,
			}
			if len(pendingReasoning) > 0 {
				msg["reasoning_content"] = strings.Join(pendingReasoning, "\n")
				pendingReasoning = nil
			}
			messages = append(messages, msg)
		}

		for i, itemAny := range inp {
			switch item := itemAny.(type) {
			case string:
				flushPendingAssistant()
				if strings.TrimSpace(item) != "" {
					messages = append(messages, map[string]any{
						"role":    "user",
						"content": item,
					})
				}
			case map[string]any:
				if item == nil {
					continue
				}
				itemType, _ := item["type"].(string)
				role, _ := item["role"].(string)

				switch itemType {
				case "function_call":
					callID, _ := item["call_id"].(string)
					if callID == "" {
						callID, _ = item["id"].(string)
					}
					if callID == "" {
						callID = fmt.Sprintf("call_%d", i+1)
					}
					name, _ := item["name"].(string)
					if callID != "" && name != "" {
						callIDToName[callID] = name
					}
					argsStr := "{}"
					if a, ok := item["arguments"].(string); ok && a != "" {
						argsStr = a
					} else if aObj, ok := item["arguments"].(map[string]any); ok {
						b, _ := json.Marshal(aObj)
						argsStr = string(b)
					}
					pendingToolCalls = append(pendingToolCalls, map[string]any{
						"id":   callID,
						"type": "function",
						"function": map[string]any{
							"name":      name,
							"arguments": argsStr,
						},
					})
					continue

				case "function_call_output":
					flushPendingAssistant()
					callID, _ := item["call_id"].(string)
					if callID == "" {
						callID, _ = item["id"].(string)
					}
					name, _ := item["name"].(string)
					if name == "" && callID != "" {
						name = callIDToName[callID]
					}
					var outStr string
					switch o := item["output"].(type) {
					case string:
						outStr = o
					case nil:
						outStr = ""
					default:
						b, _ := json.Marshal(o)
						outStr = string(b)
					}
					messages = append(messages, map[string]any{
						"role":         "tool",
						"tool_call_id": callID,
						"name":         name,
						"content":      outStr,
					})
					continue

				case "reasoning":
					if sumArr, ok := item["summary"].([]any); ok {
						for _, sAny := range sumArr {
							if sMap, ok := sAny.(map[string]any); ok {
								if t, _ := sMap["text"].(string); strings.TrimSpace(t) != "" {
									pendingReasoning = append(pendingReasoning, strings.TrimSpace(t))
								}
							}
						}
					} else if t, _ := item["text"].(string); strings.TrimSpace(t) != "" {
						pendingReasoning = append(pendingReasoning, strings.TrimSpace(t))
					}
					continue

				case "input_text", "input_image", "image":
					flushPendingAssistant()
					messages = append(messages, map[string]any{
						"role":    "user",
						"content": []any{item},
					})
					continue
				}

				flushPendingAssistant()
				if role == "" {
					role = "user"
				}
				if role == "assistant" || role == "model" {
					msg := normalizeResponsesAssistantMessage(item, i)
					if len(pendingReasoning) > 0 {
						existing, _ := msg["reasoning_content"].(string)
						if existing != "" {
							msg["reasoning_content"] = strings.Join(pendingReasoning, "\n") + "\n" + existing
						} else {
							msg["reasoning_content"] = strings.Join(pendingReasoning, "\n")
						}
						pendingReasoning = nil
					}
					if tcs, ok := msg["tool_calls"].([]any); ok {
						for _, tcAny := range tcs {
							if tcMap, ok := tcAny.(map[string]any); ok {
								cid, _ := tcMap["id"].(string)
								if fnMap, ok := tcMap["function"].(map[string]any); ok {
									if fnName, _ := fnMap["name"].(string); cid != "" && fnName != "" {
										callIDToName[cid] = fnName
									}
								}
							}
						}
					}
					messages = append(messages, msg)
				} else {
					messages = append(messages, map[string]any{
						"role":    role,
						"content": normalizeResponsesContentParts(item["content"]),
					})
				}
			}
		}
		flushPendingAssistant()
	}

	if len(messages) == 0 {
		return nil, nil, nil, transport.NewUpstreamError("input must contain at least one message or non-empty string", 400, "invalid_request_error", nil, nil)
	}

	tools = normalizeResponsesTools(body["tools"])
	toolChoice = normalizeResponsesToolChoice(body["tool_choice"])
	return messages, tools, toolChoice, nil
}

func normalizeResponsesAssistantMessage(item map[string]any, idx int) map[string]any {
	var textParts []string
	var reasoningParts []string
	var toolCalls []any

	if tcs, ok := item["tool_calls"].([]any); ok {
		toolCalls = append(toolCalls, tcs...)
	}

	switch cp := item["content"].(type) {
	case string:
		if cp != "" {
			textParts = append(textParts, cp)
		}
	case []any:
		for cIdx, cAny := range cp {
			cMap, ok := cAny.(map[string]any)
			if !ok || cMap == nil {
				continue
			}
			cType, _ := cMap["type"].(string)
			switch cType {
			case "output_text", "text", "input_text":
				if t, _ := cMap["text"].(string); t != "" {
					textParts = append(textParts, t)
				}
			case "reasoning", "summary_text", "thinking":
				if t, _ := cMap["text"].(string); t != "" {
					reasoningParts = append(reasoningParts, t)
				} else if t, _ := cMap["thinking"].(string); t != "" {
					reasoningParts = append(reasoningParts, t)
				}
			case "function_call", "tool_use":
				callID, _ := cMap["call_id"].(string)
				if callID == "" {
					callID, _ = cMap["id"].(string)
				}
				if callID == "" {
					callID = fmt.Sprintf("call_%d_%d", idx+1, cIdx+1)
				}
				name, _ := cMap["name"].(string)
				argsStr := "{}"
				if a, ok := cMap["arguments"].(string); ok && a != "" {
					argsStr = a
				} else if aObj, ok := cMap["arguments"].(map[string]any); ok {
					b, _ := json.Marshal(aObj)
					argsStr = string(b)
				} else if inObj, ok := cMap["input"].(map[string]any); ok {
					b, _ := json.Marshal(inObj)
					argsStr = string(b)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": argsStr,
					},
				})
			}
		}
	}

	var content any
	if len(textParts) > 0 {
		content = strings.Join(textParts, "\n")
	}
	msg := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if len(reasoningParts) > 0 {
		msg["reasoning_content"] = strings.Join(reasoningParts, "\n")
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

func normalizeResponsesContentParts(raw any) any {
	arr, ok := raw.([]any)
	if !ok {
		return raw
	}
	out := make([]any, 0, len(arr))
	for _, pAny := range arr {
		pMap, ok := pAny.(map[string]any)
		if !ok || pMap == nil {
			out = append(out, pAny)
			continue
		}
		pType, _ := pMap["type"].(string)
		if pType == "output_text" {
			t, _ := pMap["text"].(string)
			out = append(out, map[string]any{"type": "text", "text": t})
			continue
		}
		out = append(out, pMap)
	}
	return out
}

func normalizeResponsesTools(raw any) any {
	arr, ok := raw.([]any)
	if !ok {
		return raw
	}
	out := make([]any, 0, len(arr))
	for _, tAny := range arr {
		tMap, ok := tAny.(map[string]any)
		if !ok || tMap == nil {
			out = append(out, tAny)
			continue
		}
		if _, hasFn := tMap["function"].(map[string]any); hasFn {
			out = append(out, tMap)
			continue
		}
		name, _ := tMap["name"].(string)
		if name != "" {
			fnObj := map[string]any{
				"name": name,
			}
			if desc, ok := tMap["description"].(string); ok {
				fnObj["description"] = desc
			}
			if params, exists := tMap["parameters"]; exists {
				fnObj["parameters"] = params
			} else if schema, exists := tMap["input_schema"]; exists {
				fnObj["input_schema"] = schema
			}
			out = append(out, map[string]any{
				"type":     "function",
				"function": fnObj,
			})
			continue
		}
		out = append(out, tMap)
	}
	return out
}

func normalizeResponsesToolChoice(raw any) any {
	tcMap, ok := raw.(map[string]any)
	if !ok || tcMap == nil {
		return raw
	}
	if _, hasFn := tcMap["function"].(map[string]any); hasFn {
		return tcMap
	}
	tStr, _ := tcMap["type"].(string)
	name, _ := tcMap["name"].(string)
	if tStr == "function" && name != "" {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name,
			},
		}
	}
	return raw
}

func buildResponsesUsage(chatUsage map[string]any) map[string]any {
	inTok, _ := chatUsage["prompt_tokens"].(int)
	outTok, _ := chatUsage["completion_tokens"].(int)
	totTok, _ := chatUsage["total_tokens"].(int)
	res := map[string]any{
		"input_tokens":  inTok,
		"output_tokens": outTok,
		"total_tokens":  totTok,
	}
	if details, ok := chatUsage["completion_tokens_details"].(map[string]any); ok {
		if rTok, ok := details["reasoning_tokens"].(int); ok && rTok > 0 {
			res["output_tokens_details"] = map[string]any{
				"reasoning_tokens": rTok,
			}
		}
	}
	return res
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// HandleResponses executes an OpenAI Responses API (`POST /v1/responses`) request
// with real-time SSE streaming, reasoning summary events, tool call events, session
// continuation caching, and 1M-token context offloading.
func (s *Server) HandleResponses(ctx context.Context, w http.ResponseWriter, body map[string]any, scope AuthScope) {
	if body == nil {
		sendChatError(w, false, transport.NewUpstreamError("request body must be a JSON object", 400, "invalid_request_error", nil, nil))
		return
	}
	if mRaw, exists := body["model"]; exists && mRaw != nil {
		if _, ok := mRaw.(string); !ok {
			sendChatError(w, false, transport.NewUpstreamError("model must be a string", 400, "invalid_request_error", nil, nil))
			return
		}
	}

	messages, toolsRaw, toolChoiceRaw, normErr := NormalizeResponsesInput(body)
	if normErr != nil {
		sendChatError(w, false, normErr)
		return
	}

	prep, prepErr := s.prepareGeneration(ctx, body, scope, messages, toolsRaw, toolChoiceRaw)
	if prepErr != nil {
		sendChatError(w, false, prepErr)
		return
	}

	accts := prep.accts
	account := prep.account
	model := prep.model
	mid := prep.mid
	requested := prep.requested
	hasTools := prep.hasTools
	stream := prep.stream
	buffered := prep.buffered
	opts := prep.opts
	fullPrompt := prep.fullPrompt
	curPrompt := prep.curPrompt
	overflowParts := prep.overflowParts
	imageParts := prep.imageParts
	fullAttachmentParts := prep.fullAttachmentParts
	attachmentParts := prep.attachmentParts
	hasher := prep.hasher
	respID := "resp_" + randomHex(8)
	rsID := "rs_" + randomHex(6)
	msgID := "msg_" + randomHex(6)
	created := time.Now().Unix()

	headersSent := false
	var (
		streamMu       sync.Mutex
		seqNum         int
		reasoningAdded bool
		messageAdded   bool
	)
	flusher, _ := w.(http.Flusher)

	emitSSE := func(eventType string, payload map[string]any) {
		if ctx.Err() != nil {
			return
		}
		streamMu.Lock()
		defer streamMu.Unlock()
		seqNum++
		ev := make(map[string]any, len(payload)+2)
		for k, v := range payload {
			ev[k] = v
		}
		ev["type"] = eventType
		ev["sequence_number"] = seqNum
		b, _ := json.Marshal(ev)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(b))
		if flusher != nil {
			flusher.Flush()
		}
	}

	var keepaliveStop chan struct{}
	stopKeepalive := func() {
		streamMu.Lock()
		if keepaliveStop != nil {
			close(keepaliveStop)
			keepaliveStop = nil
		}
		streamMu.Unlock()
	}
	defer stopKeepalive()

	startStream := func() {
		streamMu.Lock()
		if headersSent || ctx.Err() != nil {
			streamMu.Unlock()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		headersSent = true
		keepaliveStop = make(chan struct{})
		kaCh := keepaliveStop
		streamMu.Unlock()

		baseResp := map[string]any{
			"id":         respID,
			"object":     "response",
			"created_at": created,
			"model":      mid,
			"status":     "in_progress",
			"output":     []any{},
			"usage":      nil,
		}
		emitSSE("response.created", map[string]any{"response": baseResp})
		emitSSE("response.in_progress", map[string]any{"response": baseResp})

		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-kaCh:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					streamMu.Lock()
					_, _ = io.WriteString(w, ": ping\n\n")
					if flusher != nil {
						flusher.Flush()
					}
					streamMu.Unlock()
				}
			}
		}()
	}

	emitReasoningDelta := func(delta string) {
		if delta == "" {
			return
		}
		startStream()
		if !reasoningAdded {
			reasoningAdded = true
			emitSSE("response.output_item.added", map[string]any{
				"output_index": 0,
				"item": map[string]any{
					"id":      rsID,
					"type":    "reasoning",
					"summary": []any{},
				},
			})
			emitSSE("response.reasoning_summary_part.added", map[string]any{
				"item_id":       rsID,
				"output_index":  0,
				"summary_index": 0,
				"part": map[string]any{
					"type": "summary_text",
					"text": "",
				},
			})
		}
		emitSSE("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       rsID,
			"output_index":  0,
			"summary_index": 0,
			"delta":         delta,
		})
	}

	emitTextDelta := func(delta string) {
		if delta == "" {
			return
		}
		startStream()
		outIdx := 0
		if reasoningAdded {
			outIdx = 1
		}
		if !messageAdded {
			messageAdded = true
			emitSSE("response.output_item.added", map[string]any{
				"output_index": outIdx,
				"item": map[string]any{
					"id":      msgID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []any{},
				},
			})
			emitSSE("response.content_part.added", map[string]any{
				"item_id":       msgID,
				"output_index":  outIdx,
				"content_index": 0,
				"part": map[string]any{
					"type":        "output_text",
					"text":        "",
					"annotations": []any{},
				},
			})
		}
		emitSSE("response.output_text.delta", map[string]any{
			"item_id":       msgID,
			"output_index":  outIdx,
			"content_index": 0,
			"delta":         delta,
		})
	}

	triedAccounts := map[string]bool{account.ID: true}
	for attempt := range 2 {
		var (
			candidateID           string
			selected              bool
			done                  bool
			text, thoughts        string
			sentText, sentThought string
			servedID, servedLabel string
			identityConflict      bool
			lastCID, lastRID      string
			lastContextToken      string
			attemptStateMu        sync.Mutex
		)
		observeIdentity := func(ansID, ansLabel string) {
			if ansID == "" {
				return
			}
			if servedID != "" && servedID != ansID {
				identityConflict = true
			}
			servedID = ansID
			if ansLabel != "" {
				servedLabel = ansLabel
			}
		}
		c := s.clientFor(account)
		if attempt > 0 {
			opts.FreshSocket = true
			_ = c.Bootstrap(ctx)
		}

		files, uploadErr := s.uploadAttachments(ctx, account.ID, c, attachmentParts)
		if len(files) > 0 {
			opts.Files = files
		}

		var upstream *gemini.StreamResponse
		var streamErr error
		if uploadErr != nil {
			streamErr = uploadErr
		} else {
			upstream, streamErr = c.StreamGenerateLive(ctx, curPrompt, opts, func(update *gemini.LineParseResult) error {
				attemptStateMu.Lock()
				observeIdentity(update.AnsweredModelID, update.AnsweredModelLabel)
				earlyMismatch := s.Cfg.RejectDowngrade && (servedID != "" || servedLabel != "") && !gemini.IsMatchingServedModel(*model, servedID, servedLabel)
				attemptStateMu.Unlock()
				if earlyMismatch {
					return transport.ErrStopStreaming
				}

				if update.CID != "" {
					lastCID = update.CID
				}
				if update.RID != "" {
					lastRID = update.RID
				}
				if update.ContextToken != "" {
					lastContextToken = update.ContextToken
				}
				for _, frame := range update.Frames {
					if !selected {
						candidateID = frame.RCID
						selected = true
					}
					if frame.RCID != candidateID {
						continue
					}
					attemptStateMu.Lock()
					observeIdentity(frame.AnsweredModelID, frame.AnsweredModelLabel)
					if frame.Text != "" || text == "" {
						if !shouldPreserveTextOnErrorFrame(text, frame.Text, sentText, hasTools, toolsRaw, toolChoiceRaw, nil) {
							text = frame.Text
						}
					}
					thoughts = mergeStreamedThoughts(thoughts, frame.Thoughts)
					done = frame.Done
					frameMismatch := s.Cfg.RejectDowngrade && (servedID != "" || servedLabel != "") && !gemini.IsMatchingServedModel(*model, servedID, servedLabel)
					attemptStateMu.Unlock()
					if frameMismatch {
						return transport.ErrStopStreaming
					}

					if stream && !buffered && ctx.Err() == nil {
						if !strings.HasPrefix(text, sentText) || (s.Cfg.LiveThoughts && !strings.HasPrefix(thoughts, sentThought)) {
							return transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
						}
						if s.Cfg.LiveThoughts && len(thoughts) > len(sentThought) {
							emitReasoningDelta(thoughts[len(sentThought):])
							sentThought = thoughts
						}
						if len(text) > len(sentText) {
							emitTextDelta(text[len(sentText):])
							sentText = text
						}
					} else if stream && buffered && ctx.Err() == nil && !done {
						if sentText != "" && !strings.HasPrefix(text, sentText) {
							return transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
						}
						// Thoughts are normalized monotonically via mergeStreamedThoughts.
						identityVerified := !s.Cfg.RejectDowngrade || (!identityConflict && (servedID == "" || gemini.IsMatchingServedModel(*model, servedID, servedLabel)))
						if identityVerified && s.Cfg.LiveThoughts {
							if len(thoughts) > len(sentThought) && strings.HasPrefix(thoughts, sentThought) {
								emitReasoningDelta(thoughts[len(sentThought):])
								sentThought = thoughts
							}
						}
						canStreamText := !s.Cfg.RejectDowngrade || (!identityConflict && servedID != "" && gemini.IsMatchingServedModel(*model, servedID, servedLabel))
						if canStreamText && s.Cfg.LiveThoughts {
							if !hasTools && strings.HasPrefix(text, sentText) && len(text) > 240 && len(text) > len(sentText) && gemini.GeminiErrorReply(text) == "" {
								emitTextDelta(text[len(sentText):])
								sentText = text
							} else if hasTools && s.Cfg.KeepToolPreamble {
								if safePrefix := toolbridge.SafeStreamablePrefix(text); len(safePrefix) > len(sentText) && strings.HasPrefix(safePrefix, sentText) && gemini.GeminiErrorReply(text[:240]) == "" {
									emitTextDelta(safePrefix[len(sentText):])
									sentText = safePrefix
								}
							}
						}
					}
				}
				return nil
			})
		}

		interpreted, runCheck := s.evaluateAttemptCompletion(
			ctx,
			account,
			*model,
			mid,
			upstream,
			streamErr,
			selected,
			done,
			text,
			sentText,
			servedID,
			servedLabel,
			identityConflict,
			hasTools,
			toolsRaw,
			toolChoiceRaw,
		)

		if runCheck != nil {
			var ue *transport.UpstreamError
			errors.As(runCheck, &ue)
			errCode := ""
			errStatus := 502
			if ue != nil {
				errCode = ue.Code
				errStatus = ue.StatusCode
			}
			if errCode != "session_busy" && errCode != "client_cancelled" {
				s.markAccountFailure(account, runCheck.Error())
				s.refreshQuotaAsync(account, c)
			}
			if attempt == 0 && !headersSent && sentText == "" && sentThought == "" && ctx.Err() == nil {
				if errCode == "model_substitution" || errStatus == 429 || errCode == "rate_limit_error" {
					if scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							account = nextAcct
							model = nextModel
							opts.Model = nextModel
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							continue
						}
					}
				}
				if errStatus == 401 || errCode == "upstream_auth_error" {
					c.InvalidateAuth()
					continue
				}
				if errCode == "upstream_connect_timeout" ||
					errCode == "upstream_stalled" ||
					errCode == "upstream_timeout" {
					if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, requested, hasher); nextAcct != nil && nextModel != nil {
						triedAccounts[nextAcct.ID] = true
						account = nextAcct
						model = nextModel
						opts.Model = nextModel
						opts.Metadata = nil
						opts.Files = nil
						curPrompt = fullPrompt
						attachmentParts = fullAttachmentParts
						continue
					}
				}
				if errCode == "upstream_transport_error" ||
					errCode == "incomplete_upstream_response" ||
					errCode == "tool_call_error" {
					opts.Metadata = nil
					opts.Files = nil
					curPrompt = fullPrompt
					attachmentParts = fullAttachmentParts
					continue
				}
				if errCode == "upstream_error_reply" || errCode == "empty_upstream_response" {
					wasCachedContinuation := len(opts.Metadata) > 0
					opts.Metadata = nil
					opts.Files = nil
					if !wasCachedContinuation {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							account = nextAcct
							model = nextModel
							opts.Model = nextModel
						}
					}
					if len(fullPrompt) > 120000 {
						if compacted, extraOverflow, cErr := prompt.FlattenMessages(messages, toolsRaw, toolChoiceRaw, prompt.FlattenOptions{
							StartIndex:      0,
							MaxPromptChars:  100000,
							MaxOldToolChars: 12000,
							AllowOverflow:   true,
						}); cErr == nil {
							curPrompt = compacted
							attachmentParts = make([]prompt.AttachmentPart, 0, len(extraOverflow)+len(imageParts))
							attachmentParts = append(attachmentParts, extraOverflow...)
							attachmentParts = append(attachmentParts, imageParts...)
							continue
						}
					}
					curPrompt = fullPrompt
					attachmentParts = fullAttachmentParts
					continue
				}
			}

			stopKeepalive()
			if ctx.Err() == nil {
				sendChatError(w, headersSent, runCheck)
			}
			return
		}

		// Success
		var calls []toolbridge.Call
		if interpreted != nil {
			calls = interpreted.Calls
		}
		var contentStr string
		if len(calls) > 0 {
			if s.Cfg.KeepToolPreamble && interpreted.Text != "" {
				contentStr = interpreted.Text
			} else if s.Cfg.LiveThoughts && interpreted != nil && strings.TrimSpace(interpreted.Text) != "" {
				thoughts = appendPreambleToThoughts(thoughts, interpreted.Text)
			}
		} else {
			contentStr = text
		}

		oaCalls := toolbridge.ToOpenAIToolCalls(calls)

		chatUsage := s.computeChatUsage(fullPrompt, overflowParts, messages, toolsRaw, toolChoiceRaw, text, thoughts)
		respUsage := buildResponsesUsage(chatUsage)

		if !opts.Temporary {
			var cachedContent any = contentStr
			if len(oaCalls) > 0 && contentStr == "" {
				cachedContent = nil
			}
			s.saveSessionContinuation(account.ID, mid, messages, toolsRaw, toolChoiceRaw, cachedContent, oaCalls, lastCID, lastRID, candidateID, lastContextToken)
		}

		stopKeepalive()
		if ctx.Err() != nil {
			return
		}

		var outputItems []map[string]any
		if s.Cfg.LiveThoughts && thoughts != "" {
			outputItems = append(outputItems, map[string]any{
				"id":   rsID,
				"type": "reasoning",
				"summary": []map[string]any{
					{
						"type": "summary_text",
						"text": thoughts,
					},
				},
			})
		}
		if contentStr != "" || len(oaCalls) == 0 {
			outputItems = append(outputItems, map[string]any{
				"id":     msgID,
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]any{
					{
						"type":        "output_text",
						"text":        contentStr,
						"annotations": []any{},
					},
				},
			})
		}
		fcIDs := make([]string, len(oaCalls))
		for idx, tc := range oaCalls {
			fcID := fmt.Sprintf("fc_%s_%d", randomHex(4), idx+1)
			fcIDs[idx] = fcID
			outputItems = append(outputItems, map[string]any{
				"id":        fcID,
				"type":      "function_call",
				"call_id":   tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
				"status":    "completed",
			})
		}

		if stream {
			startStream()
			if s.Cfg.LiveThoughts && thoughts != "" {
				if len(thoughts) > len(sentThought) {
					emitReasoningDelta(thoughts[len(sentThought):])
				}
				emitSSE("response.reasoning_summary_text.done", map[string]any{
					"item_id":       rsID,
					"output_index":  0,
					"summary_index": 0,
					"text":          thoughts,
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": 0,
					"item":         outputItems[0],
				})
			}

			nextOutIdx := 0
			if s.Cfg.LiveThoughts && thoughts != "" {
				nextOutIdx = 1
			}
			if contentStr != "" || len(oaCalls) == 0 {
				if len(contentStr) > len(sentText) {
					emitTextDelta(contentStr[len(sentText):])
				} else if !messageAdded {
					emitTextDelta("")
				}
				msgItem := map[string]any{
					"id":     msgID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{
							"type":        "output_text",
							"text":        contentStr,
							"annotations": []any{},
						},
					},
				}
				emitSSE("response.output_text.done", map[string]any{
					"item_id":       msgID,
					"output_index":  nextOutIdx,
					"content_index": 0,
					"text":          contentStr,
				})
				emitSSE("response.content_part.done", map[string]any{
					"item_id":       msgID,
					"output_index":  nextOutIdx,
					"content_index": 0,
					"part": map[string]any{
						"type":        "output_text",
						"text":        contentStr,
						"annotations": []any{},
					},
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": nextOutIdx,
					"item":         msgItem,
				})
				nextOutIdx++
			}

			for idx, tc := range oaCalls {
				fcID := fcIDs[idx]
				fcOutIdx := nextOutIdx + idx
				doneItem := map[string]any{
					"id":        fcID,
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
					"status":    "completed",
				}
				emitSSE("response.output_item.added", map[string]any{
					"output_index": fcOutIdx,
					"item": map[string]any{
						"id":        fcID,
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": "",
						"status":    "in_progress",
					},
				})
				emitSSE("response.function_call_arguments.delta", map[string]any{
					"item_id":      fcID,
					"output_index": fcOutIdx,
					"delta":        tc.Function.Arguments,
				})
				emitSSE("response.function_call_arguments.done", map[string]any{
					"item_id":      fcID,
					"output_index": fcOutIdx,
					"arguments":    tc.Function.Arguments,
				})
				emitSSE("response.output_item.done", map[string]any{
					"output_index": fcOutIdx,
					"item":         doneItem,
				})
			}

			emitSSE("response.completed", map[string]any{
				"response": map[string]any{
					"id":         respID,
					"object":     "response",
					"created_at": created,
					"status":     "completed",
					"model":      mid,
					"output":     outputItems,
					"usage":      respUsage,
				},
			})
		} else {
			payload := map[string]any{
				"id":         respID,
				"object":     "response",
				"created_at": created,
				"status":     "completed",
				"model":      mid,
				"output":     outputItems,
				"usage":      respUsage,
			}
			sendJSON(w, 200, payload, nil)
		}

		account.mu.Lock()
		account.Fails = 0
		account.LastUsed = time.Now()
		account.mu.Unlock()
		s.refreshQuotaAsync(account, c)
		return
	}
}
