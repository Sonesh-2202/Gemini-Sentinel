package gemini

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/sonesh-2202/gemini-sentinel/internal/cookies"
	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
)

func TestParseFrameLineSkipsNullKeepaliveAndExtractsMetadata(t *testing.T) {
	keepalive := `[["wrb.fr",null,null,null,null,[3]]]`
	r, err := ParseFrameLine(keepalive)
	if err != nil {
		t.Fatalf("unexpected error on keepalive frame: %v", err)
	}
	if r == nil || len(r.Frames) != 0 {
		t.Fatalf("expected empty frames on keepalive, got %+v", r)
	}

	quotaFrame := `[["wrb.fr",null,null,null,null,[8,null,[["type.googleapis.com/assistant.lamda.BardFrontendService",[[1037]]]]]]]`
	_, err = ParseFrameLine(quotaFrame)
	var ue *transport.UpstreamError
	if !errors.As(err, &ue) || ue.StatusCode != 429 || ue.Code != "rate_limit_error" {
		t.Fatalf("expected 429 rate_limit_error on 1037 frame, got %v", err)
	}
}

func TestParseFrameLineExtractsAnsweredModelAndThoughts(t *testing.T) {
	inner := `[null,["c_123","r_456"],null,null,[["rc_789",["Hello world"],null,null,null,null,null,null,[2],{"38":[["thinking step"]]}]],{"26":"ctx_tok_1","40":"954c0abcba24364","43":"4 Argon"}]`
	innerEsc, _ := marshalJSONString(inner)
	line := `[["wrb.fr",null,` + innerEsc + `]]`

	r, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("ParseFrameLine failed: %v", err)
	}
	if r.CID != "c_123" || r.RID != "r_456" || r.ContextToken != "ctx_tok_1" {
		t.Fatalf("unexpected ids: %+v", r)
	}
	if r.AnsweredModelID != "954c0abcba24364" || r.AnsweredModelLabel != "4 Argon" {
		t.Fatalf("unexpected answered model: id=%q label=%q", r.AnsweredModelID, r.AnsweredModelLabel)
	}
	if len(r.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(r.Frames))
	}
	f := r.Frames[0]
	if f.RCID != "rc_789" || f.Text != "Hello world" || f.Thoughts != "thinking step" || !f.Done {
		t.Fatalf("unexpected candidate frame: %+v", f)
	}
}

func TestGeminiErrorReplyHeuristics(t *testing.T) {
	if GeminiErrorReply("Sorry, something went wrong. Please try your request again.") == "" {
		t.Fatal("expected canned apology to match")
	}
	if GeminiErrorReply("Sorry, something went wrong. Please try your request again. If the problem persists, contact support.") == "" {
		t.Fatal("expected canned apology with short trailer to match")
	}
	longExplanation := "Sorry, something went wrong. Please try your request again. That message is what the upstream returns when its quota is spent, and here is the full explanation of why that happens and what to do about it in practice."
	if GeminiErrorReply(longExplanation) != "" {
		t.Fatal("expected long explanation (>200 chars) not to be mistaken for an error reply")
	}
	partialWithError := "```python\nprint(144)\n```\n\nI encountered an error doing what you asked. Could you try again?"
	if GeminiErrorReply(partialWithError) == "" {
		t.Fatal("expected appended error sentence in <=400 char reply to match")
	}
}

func TestBuildInnerRequestAndModelHeader(t *testing.T) {
	ac := NewAccountClient("acct", "SAPISID=synthetic", "", nil, nil)
	ac.SetTokens("at_tok", "bl_tok", "fsid_123")
	m := &ModelInfo{
		ModelID:       "954c0abcba24364",
		ModelName:     "gemini-argon",
		DisplayName:   "4 Argon",
		Capacity:      2,
		CapacityField: 12,
		ModelNumber:   4,
	}
	inner, uuidVal, hdr := ac.BuildInnerRequest("Hi", StreamOptions{
		Model:         m,
		ThinkingLevel: 2,
		Temporary:     true,
		DisableTools:  true,
		Metadata:      []any{"cid1", "rid1", "rcid1", nil, nil, nil, nil, nil, nil, "ctx1"},
	})
	if uuidVal == "" || !strings.Contains(inner, uuidVal) {
		t.Fatalf("expected uuid in inner payload: %s", inner)
	}
	if !strings.Contains(hdr, `"954c0abcba24364"`) || !strings.Contains(hdr, `"fsid_123"`) {
		t.Fatalf("unexpected model header: %s", hdr)
	}
}

func marshalJSONString(s string) (string, error) {
	b := strings.Builder{}
	b.WriteByte('"')
	for i := range len(s) {
		switch s[i] {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

type stubGeminiDoer struct {
	fn func(req *fhttp.Request) (*fhttp.Response, error)
}

func (d *stubGeminiDoer) Do(req *fhttp.Request) (*fhttp.Response, error) {
	return d.fn(req)
}

func (d *stubGeminiDoer) CloseIdleConnections() {}

func TestParseModelRequestAndStrictThreeThinkingLevels(t *testing.T) {
	clean, think := ParseModelRequest("sentinel/gemini-4-argon@think=high")
	if clean != "gemini-4-argon" || think != "high" {
		t.Fatalf("unexpected ParseModelRequest result: clean=%q think=%q", clean, think)
	}
	m := ModelInfo{
		ModelID:     "95a4c0abcba24364",
		ModelName:   "gemini-argon",
		DisplayName: "4 Argon",
		Aliases:     []string{"gemini-4-argon"},
	}
	for _, wanted := range []string{"gemini-argon", "gemini-4-argon", "sentinel/gemini-argon", "sentinel/gemini-4-argon@think=medium"} {
		if !MatchesModel(m, wanted) {
			t.Fatalf("expected MatchesModel to match %q", wanted)
		}
	}
	if ThinkingLevelFor("low", "gemini-argon", "low") != 1 {
		t.Fatal("expected low -> 1")
	}
	if ThinkingLevelFor("medium", "gemini-argon", "low") != 4 {
		t.Fatal("expected medium -> 4")
	}
	if ThinkingLevelFor("high", "gemini-argon", "low") != 2 {
		t.Fatal("expected high -> 2")
	}
	if ThinkingLevelFor(nil, "gemini-argon", "4") != 4 {
		t.Fatal("expected numeric defaultEffort '4' (medium) -> 4")
	}
	if ThinkingLevelFor(nil, "gemini-argon", "2") != 2 {
		t.Fatal("expected numeric defaultEffort '2' (high) -> 2")
	}
}

func TestBootstrapSelfHealsViaRotateCookiesAndStreamOmitsSAPISIDHash(t *testing.T) {
	appCalls := 0
	rotated := false
	var streamAuthHeader string

	doer := &stubGeminiDoer{
		fn: func(req *fhttp.Request) (*fhttp.Response, error) {
			u := req.URL.String()
			switch {
			case strings.HasSuffix(u, "/app"):
				appCalls++
				if !rotated {
					return &fhttp.Response{
						StatusCode: 200,
						Header:     fhttp.Header{},
						Body:       io.NopCloser(strings.NewReader(`<html>signed out</html>`)),
					}, nil
				}
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(`{"SNlM0e":"at_fresh_123","cfb2h":"boq_test","FdrFJe":"999","oPEP7c":"u@example.com"}`)),
				}, nil
			case strings.Contains(u, "RotateCookies"):
				rotated = true
				return &fhttp.Response{
					StatusCode: 200,
					Header: fhttp.Header{
						"Set-Cookie": []string{"__Secure-1PSIDTS=renewed_ts; Path=/; Secure; HttpOnly"},
					},
					Body: io.NopCloser(strings.NewReader(`[]`)),
				}, nil
			case strings.Contains(u, "StreamGenerate"):
				streamAuthHeader = req.Header.Get("authorization")
				inner := `[null,["c_1","r_1"],null,null,[["rc_1",["ok"],null,null,null,null,null,null,[2]]],{"40":"95a4c0abcba24364","43":"4 Argon"}]`
				esc, _ := marshalJSONString(inner)
				body := ")]}'\n\n" + `[["wrb.fr",null,` + esc + `]]` + "\n"
				return &fhttp.Response{
					StatusCode: 200,
					Header:     fhttp.Header{},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			default:
				return &fhttp.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
		},
	}

	pool := transport.NewPool(transport.PoolOptions{
		ClientConfig: transport.ClientConfig{CustomDoer: doer},
		MaxSessions:  2,
	})
	ac := NewAccountClient("acct1", "SAPISID=sap1; __Secure-1PSID=p1; __Secure-1PSIDTS=stale", "", pool, cookies.NewRotator())
	if err := ac.Bootstrap(context.Background()); err != nil {
		t.Fatalf("expected Bootstrap to self-heal via RotateCookies, got err: %v", err)
	}
	if appCalls != 2 || !rotated {
		t.Fatalf("expected 2 /app calls with RotateCookies in between, got appCalls=%d rotated=%v", appCalls, rotated)
	}
	_, err := ac.StreamGenerateLive(context.Background(), "hello", StreamOptions{
		Model:         &ModelInfo{ModelID: "95a4c0abcba24364", ModelName: "gemini-argon", Capacity: 2, ModelNumber: 4},
		ThinkingLevel: 1,
	}, nil)
	if err != nil {
		t.Fatalf("StreamGenerateLive failed: %v", err)
	}
	if streamAuthHeader != "" {
		t.Fatalf("expected StreamGenerate to omit Authorization header, got %q", streamAuthHeader)
	}
}

func TestParseFrameLineStripsInlineThinkAndCodeReferenceIntoThoughts(t *testing.T) {
	inner := `[null,["c_1","r_1"],null,null,[["rc_1",["<think> Planning step 1 </think>\n\n` + "```python?code_reference&code_event_index=1\\nprint(19 * 27)\\n```\\n\\n```text?code_stdout&code_event_index=1\\n513\\n```\\n\\n" + `Assistant: Final clean answer."],null,null,null,null,null,null,[2]]],{"40":"95a4c0abcba24364","43":"4 Argon"}]`
	esc, _ := marshalJSONString(inner)
	line := `[["wrb.fr",null,` + esc + `]]`
	res, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("ParseFrameLine failed: %v", err)
	}
	if len(res.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(res.Frames))
	}
	f := res.Frames[0]
	if f.Text != "Final clean answer." {
		t.Fatalf("expected clean text without <think>, code_reference, or Assistant:, got %q", f.Text)
	}
	if !strings.Contains(f.Thoughts, "Planning step 1") || !strings.Contains(f.Thoughts, "print(19 * 27)") || !strings.Contains(f.Thoughts, "513") {
		t.Fatalf("expected inline <think> and code_reference moved into Thoughts, got %q", f.Thoughts)
	}
}

func TestFinalUnclosedThinkAndEchoedHarnessReminderNeverLeak(t *testing.T) {
	rawText := "[Harness reminder: Available external tools: read_file(path).\nExternal harness JSON tool calls (`{\"tool_calls\":[{\"name\":\"<tool>\",\"arguments\":{...}}]}`) are plain-text JSON replies.\nIf more files, searches, edits, or commands are needed to complete the task, output ONLY the ```json {\"tool_calls\":[...]} ``` block.]\n\nVisible answer.<think>private trailing reasoning"
	inner := `[null,["c_1","r_1"],null,null,[["rc_1",[""],null,null,null,null,null,null,[2]]],{"40":"95a4c0abcba24364","43":"4 Argon"}]`
	inner = strings.Replace(inner, `[""]`, `[`+mustQuoteJSON(t, rawText)+`]`, 1)
	esc, _ := marshalJSONString(inner)
	line := `[["wrb.fr",null,` + esc + `]]`

	res, err := ParseFrameLine(line)
	if err != nil {
		t.Fatalf("ParseFrameLine failed: %v", err)
	}
	if len(res.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(res.Frames))
	}
	f := res.Frames[0]
	if f.Text != "Visible answer." {
		t.Fatalf("expected clean 'Visible answer.', got %q", f.Text)
	}
	if !strings.Contains(f.Thoughts, "private trailing reasoning") {
		t.Fatalf("expected trailing unclosed <think> moved to Thoughts, got %q", f.Thoughts)
	}
}

func TestParseFrameLineExtractsInlineThinkingProcessMonotonicallyAcrossStreamingFrames(t *testing.T) {
	f1Text := "Thinking Process:\n\n1.  **Analyze Current State:**\n    *   `WaterAndSky.ts` was written cleanly.\n    *   The rule violation was `public get muted(): boolean { return this.isMuted; }`.\n```json"
	f2Text := f1Text + "\n{\"tool_calls\":[{\"name\":\"edit\",\"arguments\":{\"path\":\"src/audio/NatureAudioEngine.ts\"}}]}\n```"

	buildLine := func(rawText string, status int) string {
		inner := `[null,["c_1","r_1"],null,null,[["rc_1",[""],null,null,null,null,null,null,[` + strings.TrimSpace(string(rune('0'+status))) + `]]],{"40":"95a4c0abcba24364","43":"4 Argon"}]`
		inner = strings.Replace(inner, `[""]`, `[`+mustQuoteJSON(t, rawText)+`]`, 1)
		esc, _ := marshalJSONString(inner)
		return `[["wrb.fr",null,` + esc + `]]`
	}

	res1, err := ParseFrameLine(buildLine(f1Text, 1))
	if err != nil || len(res1.Frames) != 1 {
		t.Fatalf("frame 1 parse failed: %v", err)
	}
	if res1.Frames[0].Text != "" {
		t.Fatalf("expected empty Text during streaming Thinking Process with partial fence held back, got %q", res1.Frames[0].Text)
	}
	if !strings.Contains(res1.Frames[0].Thoughts, "public get muted(): boolean { return this.isMuted; }") || strings.Contains(res1.Frames[0].Thoughts, "```json") {
		t.Fatalf("unexpected frame 1 Thoughts: %q", res1.Frames[0].Thoughts)
	}

	res2, err := ParseFrameLine(buildLine(f2Text, 2))
	if err != nil || len(res2.Frames) != 1 {
		t.Fatalf("frame 2 parse failed: %v", err)
	}
	if !strings.HasPrefix(res2.Frames[0].Thoughts, res1.Frames[0].Thoughts) {
		t.Fatalf("expected frame 2 Thoughts (%q) to have frame 1 Thoughts (%q) as prefix", res2.Frames[0].Thoughts, res1.Frames[0].Thoughts)
	}
	if strings.Contains(res2.Frames[0].Text, "Thinking Process:") || !strings.HasPrefix(res2.Frames[0].Text, "```json\n{\"tool_calls\"") {
		t.Fatalf("expected frame 2 Text to contain only the tool_calls block, got %q", res2.Frames[0].Text)
	}
}

func mustQuoteJSON(t *testing.T, s string) string {
	t.Helper()
	q, err := marshalJSONString(s)
	if err != nil {
		t.Fatalf("marshalJSONString: %v", err)
	}
	return q
}

func BenchmarkSanitizeCandidateTextAndThoughts_24KBStream(b *testing.B) {
	chunk := "export class NatureRenderer {\n  private scene: THREE.Scene;\n  public update(dt: number): void {\n    const factor = dt * 0.016;\n  }\n}\n"
	frames := make([]string, 50)
	acc := "Thinking Process:\n\n1. **Designing Scene Graph**:\n   * Keep draw calls below 45.\n   * Lock pixelRatio to 1.\n\nFinal Answer:\n"
	for i := range 50 {
		acc += strings.Repeat(chunk, 4)
		frames[i] = acc
	}
	b.ReportAllocs()
	for b.Loop() {
		for idx, f := range frames {
			_, _ = sanitizeCandidateTextAndThoughts(f, "", idx == len(frames)-1)
		}
	}
}
