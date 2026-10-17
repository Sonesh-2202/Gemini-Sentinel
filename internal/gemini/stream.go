package gemini

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
)

// UploadedFile represents the [[uploadPath], safeName] tuple expected in inner[0][3].
type UploadedFile struct {
	Path     string
	Filename string
}

// StreamOptions configures a StreamGenerate request.
type StreamOptions struct {
	Model         *ModelInfo
	ThinkingLevel int
	Temporary     bool
	Language      string
	Metadata      []any // 10-element [cid, rid, rcid, null, null, null, null, null, null, contextToken]
	Files         []UploadedFile
	DisableTools  bool
	FreshSocket   bool
}

// StreamResponse captures the final HTTP status, headers, and conversation state from StreamGenerateLive.
type StreamResponse struct {
	Status  int
	Headers map[string]string
	Empty   bool
	CID     string
	RID     string
	Body    string
}

// UploadFile uploads a binary file or image to Google's content-push service
// (with fallback to 2-step Scotrup resumable upload) and returns an UploadedFile.
func (ac *AccountClient) UploadFile(ctx context.Context, data []byte, filename, contentType string) (UploadedFile, error) {
	if len(data) == 0 {
		return UploadedFile{}, transport.NewUpstreamError("uploadFile requires a non-empty Buffer", 400, "invalid_request_error", nil, nil)
	}
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return UploadedFile{}, err
	}
	var uploaded UploadedFile
	err = sess.Lease(ctx, func(leasedCookie string) error {
		if leasedCookie != "" {
			ac.mu.Lock()
			ac.cookieHeader = leasedCookie
			ac.mu.Unlock()
		}
		var uErr error
		uploaded, uErr = ac.uploadFileLocked(ctx, sess.Client, data, filename, contentType)
		return uErr
	})
	return uploaded, err
}

func (ac *AccountClient) uploadFileLocked(ctx context.Context, sc *transport.SentinelClient, data []byte, filename, contentType string) (UploadedFile, error) {
	if filename == "" {
		filename = "image.png"
	}
	safeName := strings.Map(func(r rune) rune {
		if r == '"' || r == '\r' || r == '\n' {
			return '_'
		}
		return r
	}, filename)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ac.mu.RLock()
	pushID := ac.pushID
	pctx := ac.pctx
	cookie := ac.cookieHeader
	ac.mu.RUnlock()
	if pushID == "" {
		pushID = defaultPushID
	}

	var randHex [8]byte
	_, _ = rand.Read(randHex[:])
	boundary := "----GeminiSentinelUpload" + hex.EncodeToString(randHex[:])

	var body bytes.Buffer
	fmt.Fprintf(&body, "--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\nContent-Type: %s\r\n\r\n", boundary, safeName, contentType)
	body.Write(data)
	fmt.Fprintf(&body, "\r\n--%s--\r\n", boundary)

	res, err := sc.Do(ctx, transport.RequestOptions{
		Method: "POST",
		URL:    "https://content-push.googleapis.com/upload",
		Kind:   transport.ReqKindCORS,
		Headers: map[string]string{
			"cookie":         cookie,
			"origin":         Origin,
			"referer":        Origin + "/",
			"sec-fetch-site": "cross-site",
			"x-tenant-id":    defaultTenant,
			"push-id":        pushID,
			"content-type":   "multipart/form-data; boundary=" + boundary,
		},
		Body:    body.Bytes(),
		Timeout: 60 * time.Second,
	})
	if err == nil {
		ac.absorbAndPersistCookies(res.SetCookies)
		trimmed := strings.TrimSpace(res.Body)
		if res.Status == 200 && strings.HasPrefix(trimmed, "/") {
			return UploadedFile{Path: trimmed, Filename: safeName}, nil
		}
	}

	// Fallback path: 2-step Scotrup resumable upload via push.clients6.google.com/upload/
	initHeaders := map[string]string{
		"cookie":                              cookie,
		"origin":                              Origin,
		"referer":                             Origin + "/",
		"sec-fetch-site":                      "same-site",
		"x-tenant-id":                         defaultTenant,
		"push-id":                             pushID,
		"x-goog-upload-command":               "start",
		"x-goog-upload-protocol":              "resumable",
		"x-goog-upload-header-content-length": strconv.Itoa(len(data)),
		"content-type":                        "application/x-www-form-urlencoded;charset=UTF-8",
	}
	if pctx != "" {
		initHeaders["x-client-pctx"] = pctx
	}
	initRes, initErr := sc.Do(ctx, transport.RequestOptions{
		Method:  "POST",
		URL:     "https://push.clients6.google.com/upload/",
		Kind:    transport.ReqKindCORS,
		Headers: initHeaders,
		Body:    []byte("File name: " + safeName),
		Timeout: 30 * time.Second,
	})
	if initErr == nil {
		ac.absorbAndPersistCookies(initRes.SetCookies)
		uploadURL := initRes.Headers["x-goog-upload-url"]
		if initRes.Status == 200 && uploadURL != "" {
			putHeaders := map[string]string{
				"cookie":                cookie,
				"origin":                Origin,
				"referer":               Origin + "/",
				"sec-fetch-site":        "same-site",
				"x-tenant-id":           defaultTenant,
				"push-id":               pushID,
				"x-goog-upload-command": "upload, finalize",
				"x-goog-upload-offset":  "0",
				"content-type":          "application/x-www-form-urlencoded;charset=UTF-8",
			}
			if pctx != "" {
				putHeaders["x-client-pctx"] = pctx
			}
			putRes, putErr := sc.Do(ctx, transport.RequestOptions{
				Method:  "POST",
				URL:     uploadURL,
				Kind:    transport.ReqKindCORS,
				Headers: putHeaders,
				Body:    data,
				Timeout: 60 * time.Second,
			})
			if putErr == nil {
				ac.absorbAndPersistCookies(putRes.SetCookies)
				trimmed := strings.TrimSpace(putRes.Body)
				if putRes.Status == 200 && strings.HasPrefix(trimmed, "/") {
					return UploadedFile{Path: trimmed, Filename: safeName}, nil
				}
			}
		}
	}

	if err != nil {
		return UploadedFile{}, err
	}
	status := 502
	code := "upstream_upload_error"
	if res.Status == 401 || res.Status == 403 {
		status = 401
		code = "upstream_auth_error"
	}
	return UploadedFile{}, transport.NewUpstreamError(
		fmt.Sprintf("file upload failed with HTTP %d", res.Status),
		status,
		code,
		res.Headers,
		nil,
	)
}

// BuildInnerRequest constructs the 81-slot StreamGenerate inner payload, uuid,
// and x-goog-ext-525001261-jspb header value matching gemini-web.js.
func (ac *AccountClient) BuildInnerRequest(prompt string, opts StreamOptions) (innerJSON string, uuidVal string, modelHeader string) {
	ac.mu.RLock()
	fSid := ac.fSid
	ac.mu.RUnlock()

	lang := opts.Language
	if lang == "" {
		lang = "en"
	}
	meta := opts.Metadata
	if len(meta) == 0 {
		meta = []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	}

	var filesSlot any
	if len(opts.Files) > 0 {
		fArr := make([]any, 0, len(opts.Files))
		for _, f := range opts.Files {
			fArr = append(fArr, []any{[]string{f.Path}, f.Filename})
		}
		filesSlot = fArr
	}

	inner := make([]any, 81)
	inner[0] = []any{prompt, 0, nil, filesSlot, nil, nil, 0}
	inner[1] = []string{lang}
	inner[2] = meta
	inner[6] = []int{1}
	inner[7] = 1  // enable_token_streaming
	inner[10] = 1 // request_source
	if opts.DisableTools {
		inner[11] = 1 // disable_tools (strip consumer Workspace/YouTube/Maps extensions)
	} else {
		inner[11] = 0
	}
	inner[17] = [][]int{{0}}
	inner[18] = 0
	inner[27] = 1 // return_request_routing_early
	inner[30] = []int{4}
	inner[41] = []int{1}
	if opts.Temporary {
		inner[45] = 1
	}
	inner[53] = 0
	inner[61] = []any{}
	inner[67] = 1 // is_personal_context_disabled
	inner[68] = 1
	if len(meta) >= 10 {
		cid, _ := meta[0].(string)
		ctxTok, _ := meta[9].(string)
		if cid != "" && ctxTok != "" {
			inner[71] = []any{cid, nil, nil, nil, nil, nil, nil, ctxTok}
		}
	}
	inner[79] = 1
	thinkLvl := opts.ThinkingLevel
	if thinkLvl <= 0 {
		thinkLvl = 1
	}
	inner[80] = thinkLvl

	uuidVal = strings.ToUpper(uuid.NewString())
	inner[59] = uuidVal

	if opts.Model != nil {
		if opts.Model.ModelNumber > 0 {
			inner[79] = opts.Model.ModelNumber
		}
		modelHeader = BuildModelHeader(*opts.Model, thinkLvl, fSid)
	}

	b, _ := json.Marshal(inner)
	return string(b), uuidVal, modelHeader
}

// StreamGenerateLive executes a live StreamGenerate request over the account's
// multiplexed HTTP/2 session, invoking onFrame as lines arrive.
func (ac *AccountClient) StreamGenerateLive(
	ctx context.Context,
	prompt string,
	opts StreamOptions,
	onFrame func(update *LineParseResult) error,
) (*StreamResponse, error) {
	sess, err := ac.acquireSession(ctx)
	if err != nil {
		return nil, err
	}

	var result *StreamResponse
	err = sess.RunStream(ctx, func(_ string) error {
		var runErr error
		result, runErr = ac.streamGenerateLocked(ctx, sess.Client, prompt, opts, onFrame)
		return runErr
	})
	return result, err
}

func (ac *AccountClient) streamGenerateLocked(
	ctx context.Context,
	sc *transport.SentinelClient,
	prompt string,
	opts StreamOptions,
	onFrame func(update *LineParseResult) error,
) (*StreamResponse, error) {
	if err := ac.ensureBootstrapped(ctx, sc); err != nil {
		return nil, err
	}

	innerJSON, uuidVal, modelHeader := ac.BuildInnerRequest(prompt, opts)

	ac.mu.RLock()
	at := ac.at
	bl := ac.bl
	fSid := ac.fSid
	cookie := ac.cookieHeader
	ac.mu.RUnlock()

	lang := opts.Language
	if lang == "" {
		lang = "en"
	}
	q := url.Values{}
	q.Set("hl", lang)
	q.Set("_reqid", strconv.FormatInt(ac.nextReqID(), 10))
	q.Set("rt", "c")
	if bl != "" {
		q.Set("bl", bl)
	}
	if fSid != "" {
		q.Set("f.sid", fSid)
	}

	freqBytes, _ := json.Marshal([]any{nil, innerJSON})
	form := url.Values{}
	form.Set("at", at)
	form.Set("f.req", string(freqBytes))

	headers := map[string]string{
		"content-type":              "application/x-www-form-urlencoded;charset=utf-8",
		"x-same-domain":             "1",
		"x-goog-ext-525005358-jspb": fmt.Sprintf(`[%q,1]`, uuidVal),
		"x-goog-ext-73010989-jspb":  "[0]",
		"x-goog-ext-73010990-jspb":  "[0,0,0]",
		"cookie":                    cookie,
	}
	if modelHeader != "" {
		headers["x-goog-ext-525001261-jspb"] = modelHeader
	}
	var (
		lineBuf         strings.Builder
		cid, rid        string
		frameCount      int
		sawDone         bool
		sawContextToken bool
		sawStreamEnd    bool
	)

	emitLine := func(line string) error {
		parsed, err := ParseFrameLine(line)
		if err != nil {
			return err
		}
		if parsed == nil {
			return nil
		}
		if parsed.CID != "" {
			cid = parsed.CID
		} else {
			parsed.CID = cid
		}
		if parsed.RID != "" {
			rid = parsed.RID
		} else {
			parsed.RID = rid
		}
		if parsed.ContextToken != "" {
			sawContextToken = true
		}
		if parsed.StreamEnd {
			sawStreamEnd = true
		}
		for _, f := range parsed.Frames {
			if f.Done {
				sawDone = true
			}
		}
		frameCount += len(parsed.Frames)
		if onFrame != nil && (len(parsed.Frames) > 0 || parsed.AnsweredModelID != "" || parsed.CID != "" || parsed.RID != "" || parsed.ContextToken != "") {
			return onFrame(parsed)
		}
		return nil
	}

	res, err := sc.Do(ctx, transport.RequestOptions{
		Method:      "POST",
		URL:         Origin + "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?" + q.Encode(),
		Kind:        transport.ReqKindCORS,
		Headers:     headers,
		Body:        []byte(form.Encode()),
		FreshSocket: opts.FreshSocket,
		OnChunk: func(chunk string) error {
			lineBuf.WriteString(chunk)
			cur := lineBuf.String()
			lastNL := strings.LastIndexByte(cur, '\n')
			if lastNL < 0 {
				return nil
			}
			complete := cur[:lastNL]
			rem := cur[lastNL+1:]
			lineBuf.Reset()
			lineBuf.WriteString(rem)
			for _, line := range strings.Split(complete, "\n") {
				if err := emitLine(line); err != nil {
					return err
				}
				if sawStreamEnd || (sawDone && sawContextToken) {
					return transport.ErrStopStreaming
				}
			}
			return nil
		},
	})
	if err != nil {
		var ue *transport.UpstreamError
		if errors.As(err, &ue) {
			return nil, ue
		}
		return nil, err
	}
	ac.absorbAndPersistCookies(res.SetCookies)

	if res.Status == 200 {
		if tail := strings.TrimSpace(lineBuf.String()); tail != "" {
			for _, line := range strings.Split(tail, "\n") {
				if err := emitLine(line); err != nil {
					var ue *transport.UpstreamError
					if errors.As(err, &ue) && ue.Headers == nil {
						ue.Headers = res.Headers
					}
					return nil, err
				}
			}
		}
	}

	return &StreamResponse{
		Status:  res.Status,
		Headers: res.Headers,
		Empty:   frameCount == 0,
		CID:     cid,
		RID:     rid,
		Body:    res.Body,
	}, nil
}
