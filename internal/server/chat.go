package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/gemini"
	"github.com/sonesh-2202/gemini-sentinel/internal/prompt"
	"github.com/sonesh-2202/gemini-sentinel/internal/toolbridge"
	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
)

type sseChatChoice struct {
	Index        int `json:"index"`
	Delta        any `json:"delta"`
	FinishReason any `json:"finish_reason"`
}

type sseChatChunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices [1]sseChatChoice `json:"choices"`
	Usage   map[string]any   `json:"usage,omitempty"`
}

type sseDeltaContent struct {
	Content string `json:"content"`
}

type sseDeltaReasoning struct {
	ReasoningContent string `json:"reasoning_content"`
}

type sseDeltaEmpty struct{}

// HandleChat executes an OpenAI-compatible /v1/chat/completions request.
func (s *Server) HandleChat(ctx context.Context, w http.ResponseWriter, body map[string]any, scope AuthScope) {
	if body == nil {
		sendChatError(w, false, transport.NewUpstreamError("model must be a string and messages a nonempty message array", 400, "invalid_request_error", nil, nil))
		return
	}
	if mRaw, exists := body["model"]; exists && mRaw != nil {
		if _, ok := mRaw.(string); !ok {
			sendChatError(w, false, transport.NewUpstreamError("model must be a string and messages a nonempty message array", 400, "invalid_request_error", nil, nil))
			return
		}
	}
	msgsRaw, ok := body["messages"].([]any)
	if !ok || len(msgsRaw) == 0 {
		sendChatError(w, false, transport.NewUpstreamError("model must be a string and messages a nonempty message array", 400, "invalid_request_error", nil, nil))
		return
	}
	messages := make([]map[string]any, 0, len(msgsRaw))
	for _, mAny := range msgsRaw {
		m, ok := mAny.(map[string]any)
		if !ok || m == nil {
			sendChatError(w, false, transport.NewUpstreamError("model must be a string and messages a nonempty message array", 400, "invalid_request_error", nil, nil))
			return
		}
		role, ok := m["role"].(string)
		if !ok || role == "" {
			sendChatError(w, false, transport.NewUpstreamError("model must be a string and messages a nonempty message array", 400, "invalid_request_error", nil, nil))
			return
		}
		messages = append(messages, m)
	}

	toolsRaw := body["tools"]
	toolChoiceRaw := body["tool_choice"]

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
	cmplID := "chatcmpl-" + randomHex(8)
	created := time.Now().Unix()

	headersSent := false
	var streamMu sync.Mutex
	flusher, _ := w.(http.Flusher)

	var writeBuf []byte
	emit := func(delta any, finish any, usage map[string]any) {
		if ctx.Err() != nil {
			return
		}
		streamMu.Lock()
		defer streamMu.Unlock()
		chunk := sseChatChunk{
			ID:      cmplID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   mid,
			Choices: [1]sseChatChoice{
				{
					Index:        0,
					Delta:        delta,
					FinishReason: finish,
				},
			},
			Usage: usage,
		}
		b, _ := json.Marshal(&chunk)
		writeBuf = append(writeBuf[:0], "data: "...)
		writeBuf = append(writeBuf, b...)
		writeBuf = append(writeBuf, "\n\n"...)
		_, _ = w.Write(writeBuf)
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

		emit(map[string]any{"role": "assistant", "content": ""}, nil, nil)
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
					emit(map[string]any{"role": "assistant"}, nil, nil)
				}
			}
		}()
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

		attemptStartedAt := time.Now()
		lastActiveKeepaliveAt := attemptStartedAt
		var toolStreamer *toolbridge.LiveToolCallStreamer
		preambleFlushed := false
		if stream && buffered && hasTools {
			toolStreamer = toolbridge.NewLiveToolCallStreamer(toolsRaw, toolChoiceRaw, func(d map[string]any) {
				startStream()
				if !preambleFlushed {
					preambleFlushed = true
					if pre := toolbridge.ExtractPreToolPreamble(text); pre != "" {
						if s.Cfg.KeepToolPreamble {
							if len(pre) > len(sentText) && strings.HasPrefix(pre, sentText) {
								emit(sseDeltaContent{Content: pre[len(sentText):]}, nil, nil)
								sentText = pre
							}
						} else if s.Cfg.LiveThoughts {
							thoughts = appendPreambleToThoughts(thoughts, pre)
						}
					}
				}
				if s.Cfg.LiveThoughts && len(thoughts) > len(sentThought) && strings.HasPrefix(thoughts, sentThought) {
					emit(sseDeltaReasoning{ReasoningContent: thoughts[len(sentThought):]}, nil, nil)
					sentThought = thoughts
				}
				emit(d, nil, nil)
				lastActiveKeepaliveAt = time.Now()
			})
		}

		var attemptStateMu sync.Mutex
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
						if !shouldPreserveTextOnErrorFrame(text, frame.Text, sentText, hasTools, toolsRaw, toolChoiceRaw, toolStreamer) {
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
						if len(text) > len(sentText) || (s.Cfg.LiveThoughts && len(thoughts) > len(sentThought)) {
							startStream()
							if len(text) > len(sentText) {
								emit(sseDeltaContent{Content: text[len(sentText):]}, nil, nil)
								sentText = text
							}
							if s.Cfg.LiveThoughts && len(thoughts) > len(sentThought) {
								emit(sseDeltaReasoning{ReasoningContent: thoughts[len(sentThought):]}, nil, nil)
								sentThought = thoughts
							}
						}
					} else if stream && buffered && ctx.Err() == nil && !done {
						if sentText != "" && !strings.HasPrefix(text, sentText) {
							return transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
						}
						identityVerified := !s.Cfg.RejectDowngrade || (!identityConflict && (servedID == "" || gemini.IsMatchingServedModel(*model, servedID, servedLabel)))
						if identityVerified && s.Cfg.LiveThoughts {
							if len(thoughts) > len(sentThought) && strings.HasPrefix(thoughts, sentThought) {
								startStream()
								emit(sseDeltaReasoning{ReasoningContent: thoughts[len(sentThought):]}, nil, nil)
								sentThought = thoughts
								lastActiveKeepaliveAt = time.Now()
							} else if servedID != "" && time.Since(attemptStartedAt) >= 5*time.Second {
								startStream()
								if time.Since(lastActiveKeepaliveAt) >= 5*time.Second {
									emit(map[string]any{"role": "assistant"}, nil, nil)
									lastActiveKeepaliveAt = time.Now()
								}
							}
						}
						canStreamText := !s.Cfg.RejectDowngrade || (!identityConflict && servedID != "" && gemini.IsMatchingServedModel(*model, servedID, servedLabel))
						if canStreamText {
							if !hasTools && strings.HasPrefix(text, sentText) && len(text) > 240 && len(text) > len(sentText) && gemini.GeminiErrorReply(text) == "" {
								startStream()
								emit(sseDeltaContent{Content: text[len(sentText):]}, nil, nil)
								sentText = text
								lastActiveKeepaliveAt = time.Now()
							} else if hasTools && s.Cfg.KeepToolPreamble && (toolStreamer == nil || !toolStreamer.SentAny()) && len(text) > 240 {
								if safePrefix := toolbridge.SafeStreamablePrefix(text); len(safePrefix) > len(sentText) && strings.HasPrefix(safePrefix, sentText) && gemini.GeminiErrorReply(text[:240]) == "" {
									startStream()
									emit(sseDeltaContent{Content: safePrefix[len(sentText):]}, nil, nil)
									sentText = safePrefix
									lastActiveKeepaliveAt = time.Now()
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
			if attempt == 0 && sentText == "" && (toolStreamer == nil || !toolStreamer.SentAny()) && ctx.Err() == nil {
				if errCode == "model_substitution" || errStatus == 429 || errCode == "rate_limit_error" {
					if headersSent || scope.AccountID == "" {
						if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, requested, hasher); nextAcct != nil && nextModel != nil {
							triedAccounts[nextAcct.ID] = true
							account = nextAcct
							model = nextModel
							opts.Model = nextModel
							opts.Metadata = nil
							opts.Files = nil
							curPrompt = fullPrompt
							attachmentParts = fullAttachmentParts
							sentThought = ""
							continue
						}
					}
				}
				if errStatus == 401 || errCode == "upstream_auth_error" {
					c.InvalidateAuth()
					if nextAcct, nextModel := s.pickFallbackAccount(accts, triedAccounts, requested, hasher); nextAcct != nil && nextModel != nil && (headersSent || scope.AccountID == "") {
						triedAccounts[nextAcct.ID] = true
						account = nextAcct
						model = nextModel
						opts.Model = nextModel
						opts.Metadata = nil
						opts.Files = nil
						curPrompt = fullPrompt
						attachmentParts = fullAttachmentParts
					}
					sentThought = ""
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
						sentThought = ""
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
					sentThought = ""
					continue
				}
				if errCode == "upstream_error_reply" || errCode == "empty_upstream_response" {
					wasCachedContinuation := len(opts.Metadata) > 0
					opts.Metadata = nil
					opts.Files = nil
					sentThought = ""
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

		// Success!
		var calls []toolbridge.Call
		if interpreted != nil {
			calls = interpreted.Calls
		}
		var oaCalls []toolbridge.OpenAIToolCall
		finish := "stop"
		var content any = text
		if len(calls) > 0 {
			finish = "tool_calls"
			if s.Cfg.KeepToolPreamble && interpreted.Text != "" {
				content = interpreted.Text
			} else {
				if s.Cfg.LiveThoughts && interpreted != nil && strings.TrimSpace(interpreted.Text) != "" {
					thoughts = appendPreambleToThoughts(thoughts, interpreted.Text)
				}
				content = nil
			}
		}
		var served map[string]any
		if servedID != "" {
			served = map[string]any{"id": servedID, "label": servedLabel}
		}

		usage := s.computeChatUsage(fullPrompt, overflowParts, messages, toolsRaw, toolChoiceRaw, text, thoughts)

		stopKeepalive()
		if ctx.Err() != nil {
			return
		}

		if len(calls) > 0 {
			if stream && buffered {
				startStream()
				if toolStreamer == nil || !toolStreamer.SentAny() {
					if s.Cfg.LiveThoughts && len(thoughts) > len(sentThought) && strings.HasPrefix(thoughts, sentThought) {
						emit(sseDeltaReasoning{ReasoningContent: thoughts[len(sentThought):]}, nil, nil)
						sentThought = thoughts
					}
					if contentStr, ok := content.(string); ok && len(contentStr) > len(sentText) && strings.HasPrefix(contentStr, sentText) {
						emit(sseDeltaContent{Content: contentStr[len(sentText):]}, nil, nil)
						sentText = contentStr
					}
				}
				if toolStreamer != nil {
					oaCalls = toolStreamer.Finalize(calls, text)
				} else {
					oaCalls = toolbridge.ToOpenAIToolCalls(calls)
					toolbridge.StreamOpenAIToolCallChunks(func(d map[string]any) {
						emit(d, nil, nil)
					}, oaCalls)
				}
			} else {
				oaCalls = toolbridge.ToOpenAIToolCalls(calls)
			}
		}

		if !opts.Temporary {
			s.saveSessionContinuation(account.ID, mid, messages, toolsRaw, toolChoiceRaw, content, oaCalls, lastCID, lastRID, candidateID, lastContextToken)
		}

		if stream {
			startStream()
			if buffered && len(calls) == 0 {
				if s.Cfg.LiveThoughts && len(thoughts) > len(sentThought) && strings.HasPrefix(thoughts, sentThought) {
					emit(sseDeltaReasoning{ReasoningContent: thoughts[len(sentThought):]}, nil, nil)
					sentThought = thoughts
				}
				if contentStr, ok := content.(string); ok && len(contentStr) > len(sentText) && strings.HasPrefix(contentStr, sentText) {
					emit(sseDeltaContent{Content: contentStr[len(sentText):]}, nil, nil)
					sentText = contentStr
				}
			}
			if served != nil && servedID != model.ModelID {
				emit(map[string]any{"model_served": served}, nil, nil)
			}
			emit(sseDeltaEmpty{}, finish, usage)
			streamMu.Lock()
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			streamMu.Unlock()
		} else {
			msgObj := map[string]any{
				"role":    "assistant",
				"content": content,
			}
			if s.Cfg.LiveThoughts && thoughts != "" {
				msgObj["reasoning_content"] = thoughts
			}
			if len(oaCalls) > 0 {
				msgObj["tool_calls"] = oaCalls
			}
			payload := map[string]any{
				"id":      cmplID,
				"object":  "chat.completion",
				"created": created,
				"model":   mid,
				"choices": []map[string]any{
					{
						"index":         0,
						"message":       msgObj,
						"finish_reason": finish,
					},
				},
				"usage": usage,
			}
			if served != nil && servedID != model.ModelID {
				payload["requested_model"] = mid
				payload["model_served"] = served
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

func (s *Server) evaluateAttemptCompletion(
	ctx context.Context,
	account *AccountState,
	model gemini.ModelInfo,
	mid string,
	upstream *gemini.StreamResponse,
	streamErr error,
	selected bool,
	done bool,
	text string,
	sentText string,
	servedID string,
	servedLabel string,
	identityConflict bool,
	hasTools bool,
	toolsRaw any,
	toolChoiceRaw any,
) (*toolbridge.InterpretResult, error) {
	if s.Cfg.RejectDowngrade && (servedID != "" || servedLabel != "") && !gemini.IsMatchingServedModel(model, servedID, servedLabel) {
		servedName := servedLabel
		if servedName == "" {
			servedName = servedID
		}
		s.recordModelExhausted(account, model.ModelName)
		subErr := transport.NewUpstreamError(
			fmt.Sprintf("quota exceeded: requested %s unavailable on account %s; served %s", mid, account.ID, servedName),
			429,
			"model_substitution",
			nil,
			nil,
		)
		if qHdrs := s.lookupQuotaResetHeaders(ctx, account, model.ModelName); len(qHdrs) > 0 {
			subErr.Headers = qHdrs
		}
		return nil, subErr
	}
	if streamErr != nil && !errors.Is(streamErr, transport.ErrStopStreaming) {
		return nil, streamErr
	}
	if upstream == nil || upstream.Status != 200 {
		status := 502
		var hdrs map[string]string
		if upstream != nil {
			hdrs = upstream.Headers
			if upstream.Status >= 400 && upstream.Status <= 599 {
				status = upstream.Status
			}
		}
		code := "upstream_http_error"
		if status == 401 {
			code = "upstream_auth_error"
		} else if status == 429 {
			code = "rate_limit_error"
		}
		return nil, transport.NewUpstreamError(fmt.Sprintf("upstream HTTP %d", status), status, code, hdrs, nil)
	}
	if !selected {
		return nil, s.classifyEmpty(ctx, account, model.ModelName)
	}
	if !done {
		return nil, transport.NewUpstreamError("upstream ended before the selected candidate completed", 502, "incomplete_upstream_response", nil, nil)
	}
	if text == "" {
		return nil, transport.NewUpstreamError("upstream completed without an answer", 502, "empty_upstream_response", nil, nil)
	}
	if errReply := gemini.GeminiErrorReply(text); errReply != "" {
		return nil, transport.NewUpstreamError(
			fmt.Sprintf("upstream returned an error message instead of a reply: %q", errReply),
			502,
			"upstream_error_reply",
			nil,
			nil,
		)
	}
	if identityConflict {
		return nil, transport.NewUpstreamError("upstream reported conflicting served models", 502, "model_identity_conflict", nil, nil)
	}
	if s.Cfg.RejectDowngrade && servedID == "" {
		return nil, transport.NewUpstreamError("upstream did not identify the served model", 502, "model_identity_unknown", nil, nil)
	}
	if sentText != "" && !strings.HasPrefix(text, sentText) {
		return nil, transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
	}

	var interpreted *toolbridge.InterpretResult
	var err error
	if hasTools {
		interpreted, err = toolbridge.InterpretReply(text, toolsRaw, toolChoiceRaw)
		if err != nil {
			return nil, err
		}
		if sentText != "" && interpreted != nil && len(interpreted.Calls) > 0 && !strings.HasPrefix(interpreted.Text, sentText) {
			return nil, transport.NewUpstreamError("upstream revised an already streamed answer", 502, "upstream_revision", nil, nil)
		}
	}
	return interpreted, nil
}

func (s *Server) computeChatUsage(
	fullPrompt string,
	overflowParts []prompt.AttachmentPart,
	messages []map[string]any,
	toolsRaw any,
	toolChoiceRaw any,
	text string,
	thoughts string,
) map[string]any {
	usagePrompt := fullPrompt
	if len(fullPrompt) > 500000 || len(overflowParts) > 0 {
		if rawFull, _, uErr := prompt.FlattenMessages(messages, toolsRaw, toolChoiceRaw, prompt.FlattenOptions{
			StartIndex:      0,
			MaxPromptChars:  0,
			MaxOldToolChars: s.Cfg.MaxOldToolChars,
			AllowOverflow:   false,
		}); uErr == nil {
			usagePrompt = rawFull
		}
	}
	return prompt.BuildUsage(usagePrompt, text, thoughts)
}

func mergeStreamedThoughts(prev, next string) string {
	if next == "" {
		return prev
	}
	if prev == "" || strings.HasPrefix(next, prev) {
		return next
	}
	if trimmedPrev := strings.TrimRight(prev, " \t\r\n"); trimmedPrev != "" && strings.HasPrefix(next, trimmedPrev) {
		return prev + next[len(trimmedPrev):]
	}
	if strings.Contains(prev, next) {
		return prev
	}
	return appendPreambleToThoughts(prev, next)
}

func appendPreambleToThoughts(thoughts, preamble string) string {
	p := strings.TrimSpace(preamble)
	if p == "" {
		return thoughts
	}
	if strings.TrimSpace(thoughts) == "" {
		return p
	}
	if strings.Contains(thoughts, p) {
		return thoughts
	}
	if strings.HasSuffix(thoughts, "\n\n") {
		return thoughts + p
	}
	if strings.HasSuffix(thoughts, "\n") {
		return thoughts + "\n" + p
	}
	return thoughts + "\n\n" + p
}

func budgetToEffortLevel(v any) string {
	var n float64
	switch b := v.(type) {
	case float64:
		n = b
	case int:
		n = float64(b)
	case int64:
		n = float64(b)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(b), 64)
		if err != nil {
			return ""
		}
		n = f
	default:
		return ""
	}
	if n < 0 {
		return ""
	}
	if n <= 4096 {
		return "low"
	}
	if n <= 16384 {
		return "medium"
	}
	return "high"
}

func extractEffortFromBody(body map[string]any) any {
	if v, ok := body["reasoning_effort"]; ok && v != nil {
		return v
	}
	if v, ok := body["thinking_level"]; ok && v != nil {
		return v
	}
	if v, ok := body["thinkingLevel"]; ok && v != nil {
		return v
	}
	if rMap, ok := body["reasoning"].(map[string]any); ok {
		if en, ok := rMap["enabled"].(bool); ok && !en {
			return "low"
		}
		if v, ok := rMap["effort"]; ok && v != nil {
			return v
		}
		if v, ok := rMap["level"]; ok && v != nil {
			return v
		}
	}
	if tMap, ok := body["thinking"].(map[string]any); ok {
		if en, ok := tMap["enabled"].(bool); ok && !en {
			return "low"
		}
		if tType, ok := tMap["type"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(tType)) {
			case "disabled", "off", "none":
				return "low"
			}
		}
		if v, ok := tMap["effort"]; ok && v != nil {
			return v
		}
		if v, ok := tMap["thinkingLevel"]; ok && v != nil {
			return v
		}
		if v, ok := tMap["level"]; ok && v != nil {
			return v
		}
		if b, ok := tMap["budget_tokens"]; ok && b != nil {
			if eff := budgetToEffortLevel(b); eff != "" {
				return eff
			}
		}
		if tType, ok := tMap["type"].(string); ok {
			switch strings.ToLower(strings.TrimSpace(tType)) {
			case "enabled", "adaptive":
				return "high"
			}
		}
	}
	if b, ok := body["thinking_budget"]; ok && b != nil {
		if eff := budgetToEffortLevel(b); eff != "" {
			return eff
		}
	}
	if tcMap, ok := body["thinkingConfig"].(map[string]any); ok {
		if v, ok := tcMap["thinkingLevel"]; ok && v != nil {
			return v
		}
		if b, ok := tcMap["thinkingBudget"]; ok && b != nil {
			if eff := budgetToEffortLevel(b); eff != "" {
				return eff
			}
		}
	}
	if gcMap, ok := body["generationConfig"].(map[string]any); ok {
		if tcMap, ok := gcMap["thinkingConfig"].(map[string]any); ok {
			if v, ok := tcMap["thinkingLevel"]; ok && v != nil {
				return v
			}
			if b, ok := tcMap["thinkingBudget"]; ok && b != nil {
				if eff := budgetToEffortLevel(b); eff != "" {
					return eff
				}
			}
		}
	}
	if ocMap, ok := body["output_config"].(map[string]any); ok {
		if v, ok := ocMap["effort"]; ok && v != nil {
			return v
		}
	}
	return nil
}

// shouldPreserveTextOnErrorFrame protects valid streamed content or complete tool calls
// when Gemini Web appends a canned error replacement frame at the end of a stream.
func shouldPreserveTextOnErrorFrame(
	prevText string,
	nextText string,
	sentText string,
	hasTools bool,
	toolsRaw any,
	toolChoiceRaw any,
	toolStreamer *toolbridge.LiveToolCallStreamer,
) bool {
	if prevText == "" || nextText == "" {
		return false
	}
	if gemini.GeminiErrorReply(prevText) != "" || gemini.GeminiErrorReply(nextText) == "" {
		return false
	}
	if sentText != "" || (toolStreamer != nil && toolStreamer.SentAny()) {
		return true
	}
	if hasTools {
		if interp, err := toolbridge.InterpretReply(prevText, toolsRaw, toolChoiceRaw); err == nil && interp != nil && len(interp.Calls) > 0 {
			return true
		}
	}
	return false
}
