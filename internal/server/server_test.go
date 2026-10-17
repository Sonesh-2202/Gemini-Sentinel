package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/config"
	"github.com/sonesh-2202/gemini-sentinel/internal/cookies"
	"github.com/sonesh-2202/gemini-sentinel/internal/gemini"
	"github.com/sonesh-2202/gemini-sentinel/internal/prompt"
)

var (
	testModel = gemini.ModelInfo{
		ModelName:   "gemini-pro",
		ModelID:     "aaaaaaaaaaaaaaaa",
		DisplayName: "Pro",
		Capacity:    2,
		ModelNumber: 1,
	}
	lowerModelID = "bbbbbbbbbbbbbbbb"
)

type mockUpstream struct {
	streamFn func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error)
	modelsFn func(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error)
	quotaFn  func(ctx context.Context) []gemini.QuotaBucket
	uploadFn func(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error)
	bootFn   func(ctx context.Context) error
}

func (m *mockUpstream) Bootstrap(ctx context.Context) error {
	if m.bootFn != nil {
		return m.bootFn(ctx)
	}
	return nil
}

func (m *mockUpstream) AvailableModels(ctx context.Context, force bool) (*gemini.AvailableModelsResult, error) {
	if m.modelsFn != nil {
		return m.modelsFn(ctx, force)
	}
	return &gemini.AvailableModelsResult{
		Status:   200,
		Models:   []gemini.ModelInfo{testModel},
		Capacity: 2,
	}, nil
}

func (m *mockUpstream) GetQuota(ctx context.Context) []gemini.QuotaBucket {
	if m.quotaFn != nil {
		return m.quotaFn(ctx)
	}
	return nil
}

func (m *mockUpstream) UploadFile(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, data, filename, contentType)
	}
	return gemini.UploadedFile{Path: "/contrib_service/ttl_1d/" + filename, Filename: filename}, nil
}

func (m *mockUpstream) StreamGenerateLive(ctx context.Context, promptText string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
	return m.streamFn(ctx, promptText, opts, onFrame)
}

func (m *mockUpstream) RotateCookies(ctx context.Context) (*cookies.RotateResult, error) {
	return &cookies.RotateResult{Status: 200, Rotated: false}, nil
}

func (m *mockUpstream) ReloadFromDiskIfNewer() bool { return false }
func (m *mockUpstream) InvalidateAuth()             {}

func newTestHarness(relaxed, sessionCache bool) (*Server, *AccountState) {
	cfg := config.Load()
	cfg.HomeDir = "/synthetic"
	cfg.AccountTokens = map[string]string{"audit": "acct"}
	cfg.RejectDowngrade = !relaxed
	cfg.SessionCache = sessionCache
	cfg.MaxPromptChars = 850000
	cfg.MaxOldToolChars = 32000

	srv := New(cfg)
	acct := &AccountState{
		ID:       "acct",
		Email:    "acct@example.com",
		Models:   []gemini.ModelInfo{testModel},
		Capacity: 2,
		ModelsAt: time.Now(),
	}
	srv.SetAccounts([]*AccountState{acct})
	return srv, acct
}

func runChat(t *testing.T, srv *Server, acct *AccountState, client UpstreamClient, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	acct.mu.Lock()
	acct.Client = client
	acct.mu.Unlock()
	body := map[string]any{
		"model": testModel.ModelName,
		"messages": []any{
			map[string]any{"role": "user", "content": "synthetic"},
		},
	}
	for k, v := range extra {
		body[k] = v
	}
	rec := httptest.NewRecorder()
	srv.HandleChat(context.Background(), rec, body, AuthScope{OK: true, AccountID: "acct"})
	return rec
}

func makeFrame(text, thoughts string, done bool, ansID string) gemini.Frame {
	return gemini.Frame{
		RCID:            "candidate",
		Text:            text,
		Thoughts:        thoughts,
		Done:            done,
		AnsweredModelID: ansID,
	}
}

func clientFromFrames(frames []gemini.Frame) *mockUpstream {
	return &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			for _, f := range frames {
				if err := onFrame(&gemini.LineParseResult{
					CID:             "synthetic-cid",
					RID:             "synthetic-rid",
					AnsweredModelID: f.AnsweredModelID,
					Frames:          []gemini.Frame{f},
				}); err != nil {
					return nil, err
				}
			}
			return &gemini.StreamResponse{Status: 200, CID: "synthetic-cid", RID: "synthetic-rid"}, nil
		},
	}
}

func parseSSEEvents(output string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "data: {") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(line[6:]), &obj); err == nil {
				out = append(out, obj)
			}
		}
	}
	return out
}

func TestStrictStreamingRejectsModelSubstitution(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	client := clientFromFrames([]gemini.Frame{
		makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read\"}]}\n```", "", false, ""),
		makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read\"}]}\n```", "", true, lowerModelID),
	})
	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	if rec.Code != 429 {
		t.Fatalf("expected 429 on model substitution even when code block starts on frame 1, got %d (%s)", rec.Code, rec.Body.String())
	}
	var payload map[string]map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if payload["error"]["code"] != "model_substitution" {
		t.Fatalf("expected model_substitution code, got %+v", payload)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Fatalf("expected no SSE data emitted before rejection, got %s", rec.Body.String())
	}
	// Subsequent request on the same account for the same model must fail pre-flight with 429
	rec2 := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("ok", "", true, testModel.ModelID),
	}), map[string]any{"stream": true})
	if rec2.Code != 429 {
		t.Fatalf("expected pre-flight 429 after recorded model substitution, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestStrictStreamingFailsClosedWhenIdentityAbsent(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	client := clientFromFrames([]gemini.Frame{
		makeFrame("answer", "", true, ""),
	})
	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "model_identity_unknown") {
		t.Fatalf("expected 502 model_identity_unknown, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRelaxedPartialStreamingEndsWithErrorAndNoDone(t *testing.T) {
	srv, acct := newTestHarness(true, false)
	client := clientFromFrames([]gemini.Frame{
		makeFrame("partial", "", false, testModel.ModelID),
	})
	rec := runChat(t, srv, acct, client, map[string]any{"stream": true})
	if rec.Code != 200 {
		t.Fatalf("expected 200 SSE stream, got %d", rec.Code)
	}
	evts := parseSSEEvents(rec.Body.String())
	if len(evts) == 0 {
		t.Fatalf("expected SSE events, got %s", rec.Body.String())
	}
	last := evts[len(evts)-1]
	errObj, _ := last["error"].(map[string]any)
	if errObj["code"] != "incomplete_upstream_response" {
		t.Fatalf("expected incomplete_upstream_response at end of stream, got %+v", last)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatal("expected no [DONE] terminator on incomplete stream")
	}
}

func Test429StatusAndCooldownHeadersSurvive(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	calls := 0
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			calls++
			return &gemini.StreamResponse{
				Status: 429,
				Headers: map[string]string{
					"retry-after": "3600",
					"x-resets-at": "2099-01-01T00:00:00Z",
				},
			}, nil
		},
	}
	rec := runChat(t, srv, acct, client, nil)
	if rec.Code != 429 || calls != 1 {
		t.Fatalf("expected single 429 call, got status=%d calls=%d", rec.Code, calls)
	}
	if rec.Header().Get("retry-after") != "3600" || rec.Header().Get("x-resets-at") != "2099-01-01T00:00:00Z" {
		t.Fatalf("expected cooldown headers preserved, got %+v", rec.Header())
	}
}

func TestApologyDetectionAndGenuineQuotes(t *testing.T) {
	srv, acct := newTestHarness(false, false)

	// 1. Short apology -> 502 upstream_error_reply
	rec1 := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("Sorry, something went wrong. Please try your request again.", "", true, testModel.ModelID),
	}), nil)
	if rec1.Code != 502 || !strings.Contains(rec1.Body.String(), "upstream_error_reply") {
		t.Fatalf("expected 502 upstream_error_reply, got %d: %s", rec1.Code, rec1.Body.String())
	}

	// 2. Genuine answer quoting the apology -> 200 OK
	quote := `The upstream returned "Sorry, something went wrong. Please try your request again." earlier, so I retried the request and it succeeded.`
	rec2 := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame(quote, "", true, testModel.ModelID),
	}), nil)
	if rec2.Code != 200 || !strings.Contains(rec2.Body.String(), "retried the request") {
		t.Fatalf("expected 200 OK for quoted apology, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestThinkingLevelMappingAndToolCalling(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	var seenLevels []int
	client := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			seenLevels = append(seenLevels, opts.ThinkingLevel)
			_ = onFrame(&gemini.LineParseResult{
				CID:    "c1",
				RID:    "r1",
				Frames: []gemini.Frame{makeFrame(`{"tool_calls":[{"name":"read","arguments":{"path":"/tmp/x"}}]}`, "thinking", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	for _, eff := range []any{"low", "medium", "high", nil} {
		extra := map[string]any{}
		if eff != nil {
			extra["reasoning_effort"] = eff
		}
		_ = runChat(t, srv, acct, client, extra)
	}
	expected := []int{1, 4, 2, 2}
	for i := range expected {
		if seenLevels[i] != expected[i] {
			t.Fatalf("expected thinking levels %v, got %v", expected, seenLevels)
		}
	}

	// Tool call validation + preamble suppression
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
		},
	}
	rec := runChat(t, srv, acct, client, map[string]any{"tools": tools, "tool_choice": "required"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("expected 200 tool_calls completion, got %d: %s", rec.Code, rec.Body.String())
	}
}

func Test1MContextOverflowOffloadingAndSessionCache(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	var uploaded []string
	var capturedPrompt string
	var capturedFiles []gemini.UploadedFile

	client := &mockUpstream{
		uploadFn: func(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error) {
			if contentType != "text/plain" || len(data) > 340000 {
				t.Errorf("unexpected overflow attachment %s (%s, %d bytes)", filename, contentType, len(data))
			}
			uploaded = append(uploaded, string(data))
			return gemini.UploadedFile{Path: "/contrib_service/ttl_1d/" + filename, Filename: filename}, nil
		},
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			capturedPrompt = prompt
			capturedFiles = opts.Files
			_ = onFrame(&gemini.LineParseResult{
				CID:          "c_turn1",
				RID:          "r_turn1",
				ContextToken: "ctx_1",
				Frames:       []gemini.Frame{makeFrame("ok", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	var msgs []any
	msgs = append(msgs, map[string]any{"role": "user", "content": "INITIAL_ANCHOR"})
	for i := range 60 {
		msgs = append(msgs, map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{
				map[string]any{
					"id":   fmt.Sprintf("call_%d", i),
					"type": "function",
					"function": map[string]any{
						"name":      "read",
						"arguments": fmt.Sprintf(`{"path":"file_%d.go"}`, i),
					},
				},
			},
		})
		msgs = append(msgs, map[string]any{
			"role":         "tool",
			"tool_call_id": fmt.Sprintf("call_%d", i),
			"content":      fmt.Sprintf("OLD_VERBATIM_%d_", i) + strings.Repeat("x", 20000) + fmt.Sprintf("_END_%d", i),
		})
	}
	msgs = append(msgs, map[string]any{
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{
			map[string]any{
				"id":       "call_latest",
				"type":     "function",
				"function": map[string]any{"name": "read", "arguments": `{"path":"latest.go"}`},
			},
		},
	})
	recentPayload := "RECENT_INTACT_" + strings.Repeat("y", 28000) + "_TAIL"
	msgs = append(msgs, map[string]any{
		"role":         "tool",
		"tool_call_id": "call_latest",
		"content":      recentPayload,
	})

	rec := runChat(t, srv, acct, client, map[string]any{"messages": msgs})
	if rec.Code != 200 {
		t.Fatalf("expected 200 OK on 1.3MB transcript, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(capturedPrompt) > 850000 {
		t.Fatalf("expected inline prompt <= 850KB, got %d", len(capturedPrompt))
	}
	if len(uploaded) < 2 || len(capturedFiles) != len(uploaded) {
		t.Fatalf("expected >= 2 uploaded overflow files, got %d", len(uploaded))
	}
	if !strings.Contains(uploaded[0], "OLD_VERBATIM_0_"+strings.Repeat("x", 20000)+"_END_0") {
		t.Fatal("expected first tool output preserved verbatim inside conversation_history_part_1.txt")
	}
	if !strings.Contains(capturedPrompt, recentPayload) {
		t.Fatal("expected recent tool output preserved intact inline")
	}
}

func TestHTTPHandlerEndpoints(t *testing.T) {
	srv, acct := newTestHarness(false, false)
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("hello", "", true, testModel.ModelID),
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sentinel"`) {
		t.Fatalf("unexpected /health response: %d %s", rec.Code, rec.Body.String())
	}

	reqModels := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	reqModels.Header.Set("Authorization", "Bearer audit")
	recModels := httptest.NewRecorder()
	srv.ServeHTTP(recModels, reqModels)
	if recModels.Code != 200 || !strings.Contains(recModels.Body.String(), `"gemini-pro"`) {
		t.Fatalf("unexpected /v1/models response: %d %s", recModels.Code, recModels.Body.String())
	}
}

func TestAuthTokensPrefixModelResolutionAndBudgetThinking(t *testing.T) {
	cfg := config.Load()
	cfg.HomeDir = t.TempDir()
	cfg.AccountTokens = nil // test automatic sentinel-<id> and raw <id> registration
	srv := New(cfg)

	var capturedLevel int
	mock := &mockUpstream{
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			capturedLevel = opts.ThinkingLevel
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c1",
				RID:             "r1",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("pong", "thought", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200, CID: "c1", RID: "r1"}, nil
		},
	}
	acct := &AccountState{
		ID:       "acc1",
		Email:    "acc1@example.com",
		Client:   mock,
		Models:   []gemini.ModelInfo{testModel},
		Capacity: 2,
		ModelsAt: time.Now(),
	}
	srv.SetAccounts([]*AccountState{acct})

	// 1. Auth via sentinel-<id>, raw <id>, x-api-key, and model prefix sentinel/ + @think=high
	for _, token := range []string{"sentinel-acc1", "acc1"} {
		reqBody := `{"model":"sentinel/gemini-pro@think=high","messages":[{"role":"user","content":"ping"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
		req.Header.Set("x-api-key", token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("token %q failed with %d: %s", token, rec.Code, rec.Body.String())
		}
		if capturedLevel != 2 {
			t.Fatalf("expected @think=high -> ThinkingLevel 2, got %d", capturedLevel)
		}
	}

	// 2. Thinking budget_tokens -> medium (4)
	reqBudget := `{"model":"sentinel/gemini-pro","thinking":{"type":"enabled","budget_tokens":10000},"messages":[{"role":"user","content":"ping"}]}`
	reqB := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBudget))
	reqB.Header.Set("Authorization", "Bearer sentinel-acc1")
	recB := httptest.NewRecorder()
	srv.ServeHTTP(recB, reqB)
	if recB.Code != 200 || capturedLevel != 4 {
		t.Fatalf("expected budget_tokens 10000 -> ThinkingLevel 4 (medium), got status=%d level=%d", recB.Code, capturedLevel)
	}
}

func TestResponsesEndpointStreamingAndNonStreaming(t *testing.T) {
	srv, acct := newTestHarness(false, true)
	acct.mu.Lock()
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("The answer is 42.", "Calculating 6 * 7", true, testModel.ModelID),
	})
	acct.mu.Unlock()
	// 1. Non-streaming POST /v1/responses
	reqBody := `{
		"model": "sentinel/gemini-pro",
		"instructions": "Be concise.",
		"reasoning": {"effort": "high"},
		"input": "What is 6 * 7?"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer audit")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST /v1/responses non-stream failed: %d %s", rec.Code, rec.Body.String())
	}
	var respObj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &respObj); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if respObj["object"] != "response" || respObj["status"] != "completed" {
		t.Fatalf("unexpected response envelope: %+v", respObj)
	}
	outputArr, _ := respObj["output"].([]any)
	if len(outputArr) != 2 {
		t.Fatalf("expected 2 output items (reasoning + message), got %d: %+v", len(outputArr), outputArr)
	}
	// 2. Streaming POST /v1/responses with tool call
	acct.mu.Lock()
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/etc/hostname\"}}]}\n```", "Need to read /etc/hostname", true, testModel.ModelID),
	})
	acct.mu.Unlock()
	streamReqBody := `{
		"model": "sentinel/gemini-pro",
		"stream": true,
		"tools": [{
			"type": "function",
			"name": "read_file",
			"description": "Read a file",
			"parameters": {
				"type": "object",
				"properties": {"path": {"type": "string"}},
				"required": ["path"]
			}
		}],
		"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "Read /etc/hostname"}]}]
	}`
	sReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(streamReqBody))
	sReq.Header.Set("Authorization", "Bearer audit")
	sRec := httptest.NewRecorder()
	srv.ServeHTTP(sRec, sReq)
	if sRec.Code != 200 {
		t.Fatalf("POST /v1/responses stream failed: %d %s", sRec.Code, sRec.Body.String())
	}
	sseOut := sRec.Body.String()
	for _, expectedEvent := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.reasoning_summary_text.delta",
		"event: response.function_call_arguments.delta",
		"event: response.completed",
	} {
		if !strings.Contains(sseOut, expectedEvent) {
			t.Fatalf("expected SSE stream to contain %q, got:\n%s", expectedEvent, sseOut)
		}
	}
}

func TestRemoteImageURLFetchAndMagicByteMIME(t *testing.T) {
	t.Setenv("SENTINEL_ALLOW_PRIVATE_IMAGE_URLS", "1")
	// Minimal JPEG magic bytes (\xff\xd8\xff\xe0...)
	jpegBytes := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0xff, 0xd9}
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(jpegBytes)
	}))
	defer imgSrv.Close()

	var uploadedMIME, uploadedName string
	srv, acct := newTestHarness(false, false)
	client := &mockUpstream{
		uploadFn: func(ctx context.Context, data []byte, filename, contentType string) (gemini.UploadedFile, error) {
			uploadedMIME = contentType
			uploadedName = filename
			return gemini.UploadedFile{Path: "/contrib_service/ttl_1d/" + filename, Filename: filename}, nil
		},
		streamFn: func(ctx context.Context, prompt string, opts gemini.StreamOptions, onFrame func(update *gemini.LineParseResult) error) (*gemini.StreamResponse, error) {
			_ = onFrame(&gemini.LineParseResult{
				CID:             "c_img",
				RID:             "r_img",
				AnsweredModelID: testModel.ModelID,
				Frames:          []gemini.Frame{makeFrame("nice jpeg", "", true, testModel.ModelID)},
			})
			return &gemini.StreamResponse{Status: 200}, nil
		},
	}

	rec := runChat(t, srv, acct, client, map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "What is in this image?"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": imgSrv.URL + "/photo.bin"}},
				},
			},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200 OK with remote image URL, got %d: %s", rec.Code, rec.Body.String())
	}
	if uploadedMIME != "image/jpeg" || !strings.HasSuffix(uploadedName, ".jpg") {
		t.Fatalf("expected magic-byte detection to set image/jpeg and .jpg, got mime=%q name=%q", uploadedMIME, uploadedName)
	}
}

func TestBufferedStreamRevisionLiveThoughtsAndCrossModelQuota(t *testing.T) {
	// 1. Buffered stream revision after >240 chars must emit upstream_revision error and no [DONE]
	srv, acct := newTestHarness(false, false)
	revClient := clientFromFrames([]gemini.Frame{
		makeFrame(strings.Repeat("A", 300), "", false, testModel.ModelID),
		makeFrame("Corrected final answer", "", true, testModel.ModelID),
	})
	revRec := runChat(t, srv, acct, revClient, map[string]any{"stream": true})
	if !strings.Contains(revRec.Body.String(), "upstream_revision") || strings.Contains(revRec.Body.String(), "[DONE]") {
		t.Fatalf("expected upstream_revision error and no [DONE], got:\n%s", revRec.Body.String())
	}

	// 2. LiveThoughts=false must suppress reasoning_content in chat responses
	srv.Cfg.LiveThoughts = false
	noThinkRec := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("visible reply", "secret reasoning", true, testModel.ModelID),
	}), nil)
	if noThinkRec.Code != 200 || strings.Contains(noThinkRec.Body.String(), "secret reasoning") {
		t.Fatalf("expected reasoning suppressed when LiveThoughts=false, got %d: %s", noThinkRec.Code, noThinkRec.Body.String())
	}
	srv.Cfg.LiveThoughts = true

	// 3. Exhausted Flash quota bucket must not block Pro model requests
	acct.mu.Lock()
	acct.Quota = []gemini.QuotaBucket{
		{
			ID:        "1-2",
			Label:     "Flash",
			Total:     10,
			Remaining: 0,
			ResetAt:   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	}
	acct.mu.Unlock()
	proRec := runChat(t, srv, acct, clientFromFrames([]gemini.Frame{
		makeFrame("pro works", "", true, testModel.ModelID),
	}), nil)
	if proRec.Code != 200 {
		t.Fatalf("expected Pro request to succeed when only Flash quota is spent, got %d: %s", proRec.Code, proRec.Body.String())
	}

	// 4. Streaming /v1/responses function_call item.id must match between output_item.added and response.completed
	acct.mu.Lock()
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame("```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/a\"}}]}\n```", "", true, testModel.ModelID),
	})
	acct.mu.Unlock()
	sReqBody := `{
		"model": "sentinel/gemini-pro",
		"stream": true,
		"tools": [{
			"type": "function",
			"name": "read_file",
			"parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}
		}],
		"input": {"type": "message", "role": "user", "content": "read /tmp/a"}
	}`
	sReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(sReqBody))
	sReq.Header.Set("Authorization", "Bearer audit")
	sRec := httptest.NewRecorder()
	srv.ServeHTTP(sRec, sReq)
	if sRec.Code != 200 {
		t.Fatalf("expected 200 on /v1/responses, got %d: %s", sRec.Code, sRec.Body.String())
	}
	var addedID, completedID string
	for _, line := range strings.Split(sRec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev["type"] == "response.output_item.added" {
			if item, ok := ev["item"].(map[string]any); ok && item["type"] == "function_call" {
				addedID, _ = item["id"].(string)
			}
		}
		if ev["type"] == "response.completed" {
			if resp, ok := ev["response"].(map[string]any); ok {
				if outArr, ok := resp["output"].([]any); ok {
					for _, v := range outArr {
						if item, ok := v.(map[string]any); ok && item["type"] == "function_call" {
							completedID, _ = item["id"].(string)
						}
					}
				}
			}
		}
	}
	if addedID == "" || addedID != completedID {
		t.Fatalf("expected identical function_call item.id in added (%q) and completed (%q)", addedID, completedID)
	}
}

func TestStreamingToolCallNeverLeaksThinkingProcessOrSplitsPreambleAcrossToolCalls(t *testing.T) {
	srv, acct := newTestHarness(false, false)

	// Exact reproduction of session 01a14223 entry [50]: long Thinking Process containing
	// inline `{ return this.isMuted; }` and `< 12,000 triangles` followed by fenced tool_calls.
	thinkingChunk1 := "Thinking Process:\n\n1.  **Analyze Current State:**\n    *   `WaterAndSky.ts` was written cleanly.\n    *   The rule violation was `public get muted(): boolean { return this.isMuted; }` (a tiny 1-line getter) and `(window as unknown as any).webkitAudioContext`.\n" + strings.Repeat("    *    verifying `< 12,000 triangles` and `[x, z]` coordinates.\n", 8)
	thinkingFull := thinkingChunk1 + "    *   Let's fix `NatureAudioEngine.ts` now.\n"
	toolBlock := "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"src/audio/NatureAudioEngine.ts\"}}]}\n```"

	rawFrame := func(rawText string, done bool) gemini.Frame {
		status := 1
		if done {
			status = 2
		}
		q, _ := json.Marshal(rawText)
		inner := fmt.Sprintf(`[null,["c_leak","r_leak"],null,null,[["rc_1",[%s],null,null,null,null,null,null,[%d]]],{"40":%q,"43":"Pro"}]`, string(q), status, testModel.ModelID)
		esc, _ := json.Marshal(inner)
		parsed, err := gemini.ParseFrameLine(`[["wrb.fr",null,` + string(esc) + `]]`)
		if err != nil || len(parsed.Frames) != 1 {
			t.Fatalf("ParseFrameLine failed: %v", err)
		}
		return parsed.Frames[0]
	}

	acct.mu.Lock()
	acct.Client = clientFromFrames([]gemini.Frame{
		rawFrame(thinkingChunk1, false),
		rawFrame(thinkingFull+"```json", false),
		rawFrame(thinkingFull+toolBlock, true),
	})
	acct.mu.Unlock()

	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
		},
	}

	rec := runChat(t, srv, acct, acct.Client, map[string]any{
		"stream": true,
		"tools":  tools,
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	events := parseSSEEvents(rec.Body.String())
	var streamedReasoning, streamedContent string
	sawToolCall := false
	var reasoningTokens float64
	for _, ev := range events {
		if u, ok := ev["usage"].(map[string]any); ok {
			if d, ok := u["completion_tokens_details"].(map[string]any); ok {
				reasoningTokens, _ = d["reasoning_tokens"].(float64)
			}
		}
		choices, _ := ev["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0, _ := choices[0].(map[string]any)
		delta, _ := c0["delta"].(map[string]any)
		if r, ok := delta["reasoning_content"].(string); ok && r != "" {
			if sawToolCall {
				t.Fatalf("reasoning_content delta %q emitted AFTER tool_calls started", r)
			}
			streamedReasoning += r
		}
		if c, ok := delta["content"].(string); ok && c != "" {
			streamedContent += c
		}
		if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
			sawToolCall = true
		}
	}

	if streamedContent != "" {
		t.Fatalf("expected zero content leaked on tool_calls turn, got %q", streamedContent)
	}
	if !sawToolCall {
		t.Fatal("expected tool_calls delta in stream")
	}
	if !strings.Contains(streamedReasoning, "public get muted(): boolean { return this.isMuted; }") {
		t.Fatalf("expected Thinking Process streamed cleanly via reasoning_content, got %q", streamedReasoning)
	}
	if reasoningTokens <= 0 {
		t.Fatalf("expected reasoning_tokens > 0 in usage, got %v", reasoningTokens)
	}
}

func TestToolCallPreservedWhenFinalFrameOverwritesWithGeminiErrorReply(t *testing.T) {
	srv, acct := newTestHarness(false, false)

	toolJSON := "```json\n{\"tool_calls\":[{\"name\":\"bash\",\"arguments\":{\"command\":\"npm install @vitejs/plugin-react@^4.3.3\",\"i\":\"Installing compatible Vite 5 React plugin\"}},{\"name\":\"read_file\",\"arguments\":{\"path\":\"src\"}}]}\n```"
	acct.mu.Lock()
	acct.Client = clientFromFrames([]gemini.Frame{
		makeFrame(toolJSON, "Resolving peer dependency conflict for Vite 5.", false, testModel.ModelID),
		makeFrame("I seem to be encountering an error right now. Please try again later.", "Resolving peer dependency conflict for Vite 5.", true, testModel.ModelID),
	})
	acct.mu.Unlock()

	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "bash",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string"},
						"i":       map[string]any{"type": "string"},
					},
					"required": []any{"command"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "read_file",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
		},
	}

	rec := runChat(t, srv, acct, acct.Client, map[string]any{
		"stream": true,
		"tools":  tools,
	})
	if rec.Code != 200 {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "upstream_error_reply") || strings.Contains(rec.Body.String(), "i seem to be encountering an error") {
		t.Fatalf("expected trailing error replacement frame to be ignored when valid tool calls already parsed, got: %s", rec.Body.String())
	}
	events := parseSSEEvents(rec.Body.String())
	toolCallCount := 0
	sawFinishToolCalls := false
	for _, ev := range events {
		choices, _ := ev["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c0, _ := choices[0].(map[string]any)
		if c0["finish_reason"] == "tool_calls" {
			sawFinishToolCalls = true
		}
		delta, _ := c0["delta"].(map[string]any)
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tcAny := range tcs {
				tc, _ := tcAny.(map[string]any)
				if fn, ok := tc["function"].(map[string]any); ok && fn["name"] != nil {
					toolCallCount++
				}
			}
		}
	}
	if toolCallCount != 2 || !sawFinishToolCalls {
		t.Fatalf("expected 2 tool calls and finish_reason=tool_calls, got count=%d finish=%v body=%s", toolCallCount, sawFinishToolCalls, rec.Body.String())
	}
}

func TestSessionCacheDiskPersistenceAndAccountAffinity(t *testing.T) {
	tmpHome := t.TempDir()
	cfg := config.Load()
	cfg.HomeDir = tmpHome
	cfg.SessionCache = true
	cfg.RejectDowngrade = true
	cfg.AccountTokens = nil

	srv1 := New(cfg)
	acctA := &AccountState{
		ID:       "acctA",
		Email:    "a@example.com",
		Models:   []gemini.ModelInfo{testModel},
		ModelsAt: time.Now(),
	}
	acctB := &AccountState{
		ID:       "acctB",
		Email:    "b@example.com",
		Models:   []gemini.ModelInfo{testModel},
		ModelsAt: time.Now(),
	}
	srv1.SetAccounts([]*AccountState{acctA, acctB})

	history := []map[string]any{
		{"role": "user", "content": "Hello"},
		{"role": "assistant", "content": "Hi there"},
		{"role": "user", "content": "Follow up"},
	}
	srv1.saveSessionContinuation("acctB", testModel.ModelName, history[:1], nil, nil, "Hi there", nil, "cid_b", "rid_b", "rcid_b", "ctx_b")

	// Wait briefly for async disk flush to complete
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(srv1.sessionCacheDiskPath()); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// New server instance with same HomeDir must reload session-cache.json and route pool requests to acctB via affinity
	srv2 := New(cfg)
	srv2.SetAccounts([]*AccountState{acctA, acctB})
	hasher := prompt.NewHistoryHasher(history, nil, nil)
	chosen := srv2.resolveChatAccount(srv2.Accounts(), AuthScope{OK: true}, "gemini-pro", hasher)
	if chosen == nil || chosen.ID != "acctB" {
		t.Fatalf("expected SessionCache affinity to pick acctB after disk reload, got %+v", chosen)
	}
}

func BenchmarkServerHandleChatSSE_50FramesWithTools(b *testing.B) {
	srv, acct := newTestHarness(false, false)
	chunk := strings.Repeat("The quick brown fox jumps over the lazy dog in Three.js shader pipeline. ", 8)
	frames := make([]gemini.Frame, 50)
	accText := ""
	accThought := ""
	for i := range 50 {
		accThought += "Analyzing step " + strings.Repeat("x", 20) + "\n"
		if i < 49 {
			accText += chunk
			frames[i] = makeFrame(accText, accThought, false, testModel.ModelID)
		} else {
			finalTxt := "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"src/main.ts\"}}]}\n```"
			frames[i] = makeFrame(finalTxt, accThought, true, testModel.ModelID)
		}
	}
	acct.Client = clientFromFrames(frames)
	reqBody := map[string]any{
		"model":  "gemini-pro",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "Inspect src/main.ts"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "read_file",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path": map[string]any{"type": "string"},
						},
						"required": []any{"path"},
					},
				},
			},
		},
	}
	rawJSON, _ := json.Marshal(reqBody)
	b.ReportAllocs()
	for b.Loop() {
		var parsed map[string]any
		_ = json.Unmarshal(rawJSON, &parsed)
		rec := httptest.NewRecorder()
		srv.HandleChat(context.Background(), rec, parsed, AuthScope{OK: true, AccountID: acct.ID})
	}
}
