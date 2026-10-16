package gemini

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
)

// GeneratedImage represents a generated image returned inside a StreamGenerate frame.
type GeneratedImage struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	Alt   string `json:"alt,omitempty"`
}

// Frame is a single parsed candidate frame from a wrb.fr envelope.
type Frame struct {
	RCID               string           `json:"rcid"`
	Text               string           `json:"text"`
	Thoughts           string           `json:"thoughts"`
	Done               bool             `json:"done"`
	AnsweredModelID    string           `json:"answeredModelId,omitempty"`
	AnsweredModelLabel string           `json:"answeredModelLabel,omitempty"`
	GeneratedImages    []GeneratedImage `json:"generatedImages,omitempty"`
}

// LineParseResult holds the parsed contents of one StreamGenerate line.
type LineParseResult struct {
	CID                string
	RID                string
	ContextToken       string
	AnsweredModelID    string
	AnsweredModelLabel string
	StreamEnd          bool
	Frames             []Frame
}

var (
	// Google's canned apology sentences returned with HTTP 200 when a request is rejected mid-flight.
	geminiErrorReplies = []string{
		"sorry, something went wrong. please try your request again.",
		"sorry, something went wrong. please try again.",
		"something went wrong. please try your request again.",
		"an error occurred. please try again.",
		"i encountered an error doing what you asked.",
		"i encountered an error while doing what you asked.",
		"i ran into an error while doing what you asked.",
	}
	geminiErrorReplyAnywhere = []string{
		"i encountered an error doing what you asked",
		"i encountered an error while doing what you asked",
		"i ran into an error while doing what you asked",
		"i'm having a hard time fulfilling your request",
		"i am having a hard time fulfilling your request",
		"i seem to be encountering an error",
		"i seem to be having trouble",
	}
	hex16Re                 = regexp.MustCompile(`^[0-9a-f]{14,16}$`)
	googleUserContentRe     = regexp.MustCompile(`^https://lh3\.googleusercontent\.com/`)
	imageSizeSuffixRe       = regexp.MustCompile(`=s\d+(-[a-z0-9]+)*$`)
	closedThinkBlockRe      = regexp.MustCompile("(?is)(?:<(think|thinking|thought)(?:\\s[^>]*)?>\\s*([\\s\\S]*?)</(?:think|thinking|thought)\\s*>|```(?:thinking|thought)\\b[^\\n]*\\s*([\\s\\S]*?)```)\\s*")
	codeEventBlockRe        = regexp.MustCompile("(?is)```[a-z0-9_+-]*\\?code_(?:reference|stdout|stderr)&code_event_index=\\d+\\s*([\\s\\S]*?)```\\s*")
	openThinkStartRe        = regexp.MustCompile("(?is)^\\s*(?:<(?:think|thinking|thought)(?:\\s[^>]*)?>|```(?:thinking|thought)\\b|```[a-z0-9_+-]*\\?code_(?:reference|stdout|stderr)&code_event_index=\\d+)[\\s\\S]*$")
	openThinkTagOnlyRe      = regexp.MustCompile("(?is)^\\s*(?:<(?:think|thinking|thought)(?:\\s[^>]*)?>|```(?:thinking|thought)\\b[^\\n]*|```[a-z0-9_+-]*\\?code_(?:reference|stdout|stderr)&code_event_index=\\d+)\\s*")
	trailingOpenThinkRe     = regexp.MustCompile("(?is)(?:<(?:think|thinking|thought)(?:\\s[^>]*)?>|```(?:thinking|thought)\\b[^\\n]*|```[a-z0-9_+-]*\\?code_(?:reference|stdout|stderr)&code_event_index=\\d+)([\\s\\S]*)$")
	thinkingProcessHeaderRe = regexp.MustCompile("(?is)^\\s*(?:#{1,6}\\s*)?(?:\\*\\*|__)?(?:thinking\\s+process|thought\\s+process|internal\\s+reasoning)(?:\\*\\*|__)?\\s*:?(?:\\*\\*|__)?[ \\t]*\\r?\\n+")
	fencedToolStartRe       = regexp.MustCompile("(?is)```(?:json|tool_call|tool_calls|function_call|function_calls)?\\s*(?:\\{\\s*\"\\s*(?:tool_calls|toolcalls|tool_call|function_call|function_calls|functioncalls|name|tool|tool_name|function)\\s*\"\\s*:|\\[\\s*\\{\\s*\"\\s*(?:name|tool|tool_name|function)\\s*\"\\s*:)")
	xmlToolStartRe          = regexp.MustCompile("(?is)<(?:tool_call|tool_calls|function_call|function_calls)\\b")
	bareToolEnvLineRe       = regexp.MustCompile("(?is)(?:^|\\r?\\n)[ \\t]*(\\{\\s*\"\\s*(?:tool_calls|toolcalls|tool_call|function_call|function_calls|functioncalls)\\s*\"\\s*:)")
	finalAnswerMarkerRe     = regexp.MustCompile("(?im)^\\s*(?:#{1,6}\\s*)?(?:\\*\\*|__)?(?:final\\s+answer|answer|response)(?:\\*\\*|__)?\\s*:\\s*")
	harnessReminderRe       = regexp.MustCompile(`(?is)^\s*\[Harness reminder:(?:[\s\S]*?block\.\]|[^\]\n]*\])\s*`)
	assistantPrefixRe       = regexp.MustCompile(`(?i)^\s*Assistant:\s*`)
)

const (
	geminiErrorReplyMaxChars         = 200
	geminiErrorReplyAnywhereMaxChars = 400
)

// GeminiErrorReply returns the matched canned error sentence if text is one of
// Gemini Web's silent HTTP-200 failure replies, or "" if text is a genuine answer.
func GeminiErrorReply(text string) string {
	t := strings.TrimSpace(text)
	if t == "" {
		return ""
	}
	lower := strings.ToLower(t)
	if len(t) <= geminiErrorReplyAnywhereMaxChars {
		for _, s := range geminiErrorReplyAnywhere {
			if strings.Contains(lower, s) {
				return s
			}
		}
	}
	if len(t) > geminiErrorReplyMaxChars {
		return ""
	}
	for _, s := range geminiErrorReplies {
		bare := strings.TrimSuffix(s, ".")
		if lower == s || lower == bare || strings.HasPrefix(lower, bare) {
			return s
		}
	}
	return ""
}

func asciiHasPrefixFold(s, lowerPrefix string) bool {
	if len(s) < len(lowerPrefix) {
		return false
	}
	for i := 0; i < len(lowerPrefix); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lowerPrefix[i] {
			return false
		}
	}
	return true
}

func mayContainInlineThinkTag(s string) bool {
	for off := 0; off < len(s); {
		idxLt := strings.IndexByte(s[off:], '<')
		idxBt := strings.Index(s[off:], "```")
		next := -1
		isFence := false
		if idxLt >= 0 && (idxBt < 0 || idxLt <= idxBt) {
			next = off + idxLt
		} else if idxBt >= 0 {
			next = off + idxBt
			isFence = true
		}
		if next < 0 {
			return false
		}
		rest := s[next+1:]
		if isFence {
			rest = s[next+3:]
		}
		if asciiHasPrefixFold(rest, "think") || asciiHasPrefixFold(rest, "thought") {
			return true
		}
		if isFence {
			off = next + 3
		} else {
			off = next + 1
		}
	}
	return false
}

func hasBareToolEnvelopeKey(s string) bool {
	for i := 0; i+8 <= len(s); i++ {
		c := s[i]
		if c == 't' || c == 'T' {
			if asciiHasPrefixFold(s[i:], "tool_call") || asciiHasPrefixFold(s[i:], "toolcall") {
				return true
			}
		} else if c == 'f' || c == 'F' {
			if asciiHasPrefixFold(s[i:], "function_call") || asciiHasPrefixFold(s[i:], "functioncall") {
				return true
			}
		}
	}
	return false
}

func lowerHead(s string, maxBytes int) string {
	if len(s) > maxBytes {
		s = s[:maxBytes]
	}
	return strings.ToLower(s)
}

// sanitizeCandidateTextAndThoughts strips any inline <think>/<thinking>/<thought>
// blocks or echoed prompt markers from candidate text and merges extracted inline
// thoughts into the thoughts channel so reasoning never leaks into chat content.
func sanitizeCandidateTextAndThoughts(text, thoughts string, done bool) (string, string) {
	if text == "" {
		return "", thoughts
	}

	var extraThoughts []string
	appendThought := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		if thoughts != "" && strings.Contains(thoughts, t) {
			return
		}
		extraThoughts = append(extraThoughts, t)
	}

	hasThinkTag := mayContainInlineThinkTag(text)
	hasCodeTag := strings.Contains(text, "?code_")

	// 1. Extract and strip any closed <think>...</think> / <thinking>...</thinking> / ```thinking...``` blocks
	//    as well as Gemini Web's internal ```python?code_reference&code_event_index=N``` sandbox trace blocks.
	if hasThinkTag && closedThinkBlockRe.MatchString(text) {
		matches := closedThinkBlockRe.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			if len(m) > 2 && m[2] != "" {
				appendThought(m[2])
			} else if len(m) > 3 && m[3] != "" {
				appendThought(m[3])
			}
		}
		text = closedThinkBlockRe.ReplaceAllString(text, "")
		hasThinkTag = mayContainInlineThinkTag(text)
	}
	if hasCodeTag && codeEventBlockRe.MatchString(text) {
		matches := codeEventBlockRe.FindAllStringSubmatch(text, -1)
		for _, m := range matches {
			if len(m) > 1 && m[1] != "" {
				appendThought(m[1])
			}
		}
		text = codeEventBlockRe.ReplaceAllString(text, "")
		hasCodeTag = strings.Contains(text, "?code_")
	}

	trimmed := strings.TrimLeft(text, " \t\r\n")
	lowerTrimmed := lowerHead(trimmed, 48)

	// 2. If streaming (!done), hold back partial opening prefixes of <think>, <thinking>, <thought>, ```python?code_, [Harness reminder:, or Assistant:
	if !done && trimmed != "" && len(trimmed) < 40 {
		for _, prefix := range []string{
			"<think>", "<thinking>", "<thought>",
			"```thinking", "```thought", "```python?code_", "```text?code_",
			"thinking process:", "**thinking process", "### thinking process",
			"thought process:", "**thought process", "### thought process",
			"internal reasoning:", "**internal reasoning",
			"[harness reminder:", "assistant:",
		} {
			if len(lowerTrimmed) < len(prefix) && strings.HasPrefix(prefix, lowerTrimmed) {
				return "", joinThoughts(thoughts, extraThoughts)
			}
		}
	}

	// 3. Handle echoed [Harness reminder: ... at start
	if strings.HasPrefix(lowerTrimmed, "[harness reminder:") {
		if harnessReminderRe.MatchString(text) {
			text = harnessReminderRe.ReplaceAllString(text, "")
			trimmed = strings.TrimLeft(text, " \t\r\n")
			lowerTrimmed = lowerHead(trimmed, 48)
		} else if !done {
			return "", joinThoughts(thoughts, extraThoughts)
		} else {
			// Strip unclosed/truncated reminder line on final frame so scaffolding never leaks.
			if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
				text = strings.TrimLeft(trimmed[nl+1:], " \t\r\n")
			} else {
				text = ""
			}
			trimmed = strings.TrimLeft(text, " \t\r\n")
			lowerTrimmed = lowerHead(trimmed, 48)
		}
	}

	// 4. Strip echoed leading "Assistant:" prefix
	if strings.HasPrefix(lowerTrimmed, "assistant:") {
		text = assistantPrefixRe.ReplaceAllString(text, "")
	}

	// 5. Handle unclosed <think> / <thinking> / <thought> block at start or tail (even if done=true)
	if hasThinkTag || hasCodeTag {
		if openThinkStartRe.MatchString(text) {
			inner := openThinkTagOnlyRe.ReplaceAllString(text, "")
			appendThought(inner)
			return "", joinThoughts(thoughts, extraThoughts)
		}
		if loc := trailingOpenThinkRe.FindStringSubmatchIndex(text); len(loc) >= 4 {
			inner := text[loc[2]:loc[3]]
			appendThought(inner)
			text = strings.TrimRight(text[:loc[0]], " \t\r\n")
		}
	}

	// 6. Handle inline "Thinking Process:" / "Thought Process:" blocks emitted in text instead of c[37].
	if restText, extractedThought, ok := extractThinkingProcessBlock(text, done); ok {
		appendThought(extractedThought)
		text = restText
	}

	return text, joinThoughts(thoughts, extraThoughts)
}

func findInlineToolBlockStart(s string) int {
	best := -1
	if strings.Contains(s, "```") {
		if loc := fencedToolStartRe.FindStringIndex(s); len(loc) == 2 {
			best = loc[0]
		}
	}
	if strings.Contains(s, "<") {
		if loc := xmlToolStartRe.FindStringIndex(s); len(loc) == 2 && (best < 0 || loc[0] < best) {
			best = loc[0]
		}
	}
	if strings.Contains(s, "{") && hasBareToolEnvelopeKey(s) {
		if loc := bareToolEnvLineRe.FindStringSubmatchIndex(s); len(loc) >= 4 && loc[2] >= 0 && (best < 0 || loc[2] < best) {
			best = loc[2]
		}
	}
	return best
}

func holdBackTrailingToolOpener(s string) string {
	if strings.Count(s, "```")%2 == 1 {
		if lastFence := strings.LastIndex(s, "```"); lastFence >= 0 && len(s)-lastFence <= 256 {
			cut := lastFence
			for cut > 0 && (s[cut-1] == ' ' || s[cut-1] == '\t') {
				cut--
			}
			return strings.TrimRight(s[:cut], " \t\r\n")
		}
	}
	lastNL := strings.LastIndexByte(s, '\n')
	lineStart := 0
	if lastNL >= 0 {
		lineStart = lastNL + 1
	}
	tailLine := strings.TrimLeft(s[lineStart:], " \t\r")
	if tailLine != "" && (tailLine[0] == '`' || tailLine[0] == '<' || tailLine[0] == '{') {
		return strings.TrimRight(s[:lineStart], " \t\r\n")
	}
	return strings.TrimRight(s, " \t\r\n")
}

func extractThinkingProcessBlock(text string, done bool) (restText string, thought string, ok bool) {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if trimmed == "" {
		return text, "", false
	}
	c0 := trimmed[0]
	if c0 != '#' && c0 != '*' && c0 != '_' && c0 != 't' && c0 != 'T' && c0 != 'i' && c0 != 'I' {
		return text, "", false
	}
	wsPrefixLen := len(text) - len(trimmed)
	headEnd := wsPrefixLen + 160
	if headEnd > len(text) {
		headEnd = len(text)
	}
	hdrLoc := thinkingProcessHeaderRe.FindStringIndex(text[:headEnd])
	if len(hdrLoc) != 2 {
		return text, "", false
	}
	body := text[hdrLoc[1]:]
	if toolStart := findInlineToolBlockStart(body); toolStart >= 0 {
		return strings.TrimLeft(body[toolStart:], " \t\r\n"), strings.TrimSpace(body[:toolStart]), true
	}
	if !done {
		safe := holdBackTrailingToolOpener(body)
		return "", strings.TrimSpace(safe), true
	}
	if ansLoc := finalAnswerMarkerRe.FindStringIndex(body); len(ansLoc) == 2 {
		return strings.TrimSpace(body[ansLoc[1]:]), strings.TrimSpace(body[:ansLoc[0]]), true
	}
	return "", strings.TrimSpace(body), true
}
func joinThoughts(base string, extras []string) string {
	if len(extras) == 0 {
		return base
	}
	ext := strings.Join(extras, "\n\n")
	if strings.TrimSpace(base) == "" {
		return ext
	}
	if strings.HasSuffix(base, "\n\n") {
		return base + ext
	}
	if strings.HasSuffix(base, "\n") {
		return base + "\n" + ext
	}
	return base + "\n\n" + ext
}

// jspbField reads a 0-based index from a JSPB array, supporting both dense
// positional slots and the trailing sparse dictionary object at arr[len-1]
// (where keys are 1-based field numbers).
func jspbField(arr []any, index int) any {
	if len(arr) == 0 {
		return nil
	}
	if index >= 0 && index < len(arr) && arr[index] != nil {
		val := arr[index]
		if index < len(arr)-1 {
			return val
		}
		if _, isMap := val.(map[string]any); !isMap {
			return val
		}
	}
	if lastMap, ok := arr[len(arr)-1].(map[string]any); ok {
		return lastMap[strconv.Itoa(index+1)]
	}
	return nil
}

// ParseFrameLine parses a single line from a StreamGenerate response into
// {CID, RID, ContextToken, AnsweredModelID, AnsweredModelLabel, Frames}.
func ParseFrameLine(line string) (*LineParseResult, error) {
	s := strings.TrimSpace(line)
	if s == "" || s == ")]}'" || s == ")]}" || isDigits(s) {
		return nil, nil
	}
	var outer []any
	if err := json.Unmarshal([]byte(s), &outer); err != nil {
		return nil, transport.NewUpstreamError("malformed upstream stream frame", 502, "upstream_parse_error", nil, err)
	}

	res := &LineParseResult{}
	for _, partAny := range outer {
		part, ok := partAny.([]any)
		if !ok || len(part) == 0 {
			continue
		}
		tag, _ := part[0].(string)
		if tag == "er" {
			code := extractErrCode(part)
			return nil, classifyRPCErrorCode(code, part)
		}
		if tag == "e" {
			res.StreamEnd = true
			continue
		}
		if tag != "wrb.fr" {
			continue
		}

		if err := checkErrorExtension(part); err != nil {
			return nil, err
		}

		if len(part) <= 2 || part[2] == nil {
			continue
		}
		innerStr, ok := part[2].(string)
		if !ok {
			return nil, transport.NewUpstreamError("invalid upstream frame payload", 502, "upstream_parse_error", nil, nil)
		}

		var pj []any
		if err := json.Unmarshal([]byte(innerStr), &pj); err != nil {
			return nil, transport.NewUpstreamError("malformed upstream frame payload", 502, "upstream_parse_error", nil, err)
		}

		if len(pj) > 1 {
			if ids, ok := pj[1].([]any); ok {
				if len(ids) > 0 {
					if cid, ok := ids[0].(string); ok && cid != "" {
						res.CID = cid
					}
				}
				if len(ids) > 1 {
					if rid, ok := ids[1].(string); ok && rid != "" {
						res.RID = rid
					}
				}
			}
		}

		if rawCtx, ok := jspbField(pj, 25).(string); ok && rawCtx != "" {
			res.ContextToken = rawCtx
		}

		var answeredID, answeredLabel string
		if rawID, ok := jspbField(pj, 39).(string); ok && hex16Re.MatchString(rawID) {
			answeredID = rawID
			if rawLabel, ok := jspbField(pj, 42).(string); ok && rawLabel != "" {
				answeredLabel = rawLabel
			}
			res.AnsweredModelID = answeredID
			res.AnsweredModelLabel = answeredLabel
		}

		if len(pj) > 4 {
			if cands, ok := pj[4].([]any); ok {
				for _, cAny := range cands {
					c, ok := cAny.([]any)
					if !ok {
						continue
					}
					var rcid string
					if len(c) > 0 {
						rcid, _ = c[0].(string)
					}
					var text string
					if len(c) > 1 {
						if tArr, ok := c[1].([]any); ok && len(tArr) > 0 {
							text, _ = tArr[0].(string)
						}
					}
					if strings.HasPrefix(strings.TrimSpace(text), "http://googleusercontent.com/image_generation_content/") {
						text = ""
					}

					var thoughts string
					if tf, ok := jspbField(c, 37).([]any); ok && len(tf) > 0 {
						if tf0, ok := tf[0].([]any); ok && len(tf0) > 0 {
							thoughts, _ = tf0[0].(string)
						}
					}

					done := false
					if len(c) > 8 {
						if stArr, ok := c[8].([]any); ok && len(stArr) > 0 {
							if f, ok := stArr[0].(float64); ok && int(f) == 2 {
								done = true
							}
						}
					}

					images := extractGeneratedImages(c)
					text, thoughts = sanitizeCandidateTextAndThoughts(text, thoughts, done)
					if strings.TrimSpace(text) == "" && len(images) > 0 {
						var mdParts []string
						for _, img := range images {
							alt := img.Title
							if alt == "" {
								alt = img.Alt
							}
							if alt == "" {
								alt = "Generated Image"
							}
							mdParts = append(mdParts, fmt.Sprintf("![%s](%s)", alt, img.URL))
						}
						text = strings.Join(mdParts, "\n\n")
					}
					res.Frames = append(res.Frames, Frame{
						RCID:               rcid,
						Text:               text,
						Thoughts:           thoughts,
						Done:               done,
						AnsweredModelID:    answeredID,
						AnsweredModelLabel: answeredLabel,
						GeneratedImages:    images,
					})
				}
			}
		}
	}
	return res, nil
}

// BuildContinuationMetadata constructs the 10-element Gemini Web session continuation array.
func BuildContinuationMetadata(cid, rid, rcid, contextToken string) []any {
	if cid == "" {
		return nil
	}
	return []any{cid, rid, rcid, nil, nil, nil, nil, nil, nil, contextToken}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func extractGeneratedImages(c []any) []GeneratedImage {
	if len(c) <= 12 {
		return nil
	}
	c12, ok := c[12].([]any)
	if !ok || len(c12) <= 7 {
		return nil
	}
	c12_7, ok := c12[7].([]any)
	if !ok || len(c12_7) == 0 {
		return nil
	}
	items, ok := c12_7[0].([]any)
	if !ok {
		return nil
	}
	var images []GeneratedImage
	for _, itemAny := range items {
		item, ok := itemAny.([]any)
		if !ok || len(item) == 0 {
			continue
		}
		sub0, ok := item[0].([]any)
		if !ok || len(sub0) <= 3 {
			continue
		}
		sub0_3, ok := sub0[3].([]any)
		if !ok || len(sub0_3) <= 3 {
			continue
		}
		rawURL, _ := sub0_3[3].(string)
		if !googleUserContentRe.MatchString(rawURL) {
			continue
		}
		fullURL := imageSizeSuffixRe.ReplaceAllString(rawURL, "") + "=s2048"
		var title, alt string
		if len(item) > 3 {
			if meta, ok := item[3].([]any); ok {
				if len(meta) > 6 {
					title, _ = meta[6].(string)
				}
				if len(meta) > 5 {
					if altArr, ok := meta[5].([]any); ok && len(altArr) > 0 {
						alt, _ = altArr[0].(string)
					}
				}
			}
		}
		images = append(images, GeneratedImage{
			URL:   fullURL,
			Title: title,
			Alt:   alt,
		})
	}
	return images
}

func extractErrCode(entry []any) int {
	for i := len(entry) - 1; i >= 1; i-- {
		if f, ok := entry[i].(float64); ok && f > 0 {
			return int(f)
		}
	}
	return 502
}

func checkErrorExtension(entry []any) error {
	if len(entry) <= 5 {
		return nil
	}
	ext5, ok := entry[5].([]any)
	if !ok {
		return nil
	}
	// Pattern: part[5][2][0][1][0]
	if len(ext5) > 2 {
		if list, ok := ext5[2].([]any); ok {
			for _, itemAny := range list {
				item, ok := itemAny.([]any)
				if !ok || len(item) < 2 {
					continue
				}
				if sub, ok := item[1].([]any); ok && len(sub) > 0 {
					if f, ok := sub[0].(float64); ok && f > 0 {
						return classifyRPCErrorCode(int(f), entry)
					}
					if codeArr, ok := sub[0].([]any); ok && len(codeArr) > 0 {
						if f, ok := codeArr[0].(float64); ok && f > 0 {
							return classifyRPCErrorCode(int(f), entry)
						}
					}
				}
			}
		}
	}
	return nil
}

func classifyRPCErrorCode(code int, raw any) error {
	rawBytes, _ := json.Marshal(raw)
	snippet := string(rawBytes)
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	switch code {
	case 1037:
		return transport.NewUpstreamError(
			"upstream error code 1037",
			429,
			"rate_limit_error",
			nil,
			nil,
		)
	case 1060:
		return transport.NewUpstreamError(
			"upstream error code 1060",
			429,
			"rate_limit_error",
			nil,
			nil,
		)
	case 1050, 1052:
		return transport.NewUpstreamError(
			fmt.Sprintf("upstream error code %d", code),
			400,
			"model_invalid",
			nil,
			nil,
		)
	case 400:
		return transport.NewUpstreamError(
			fmt.Sprintf("upstream error code 400: %s", snippet),
			400,
			"upstream_rpc_400",
			nil,
			nil,
		)
	case 401, 403:
		return transport.NewUpstreamError(
			fmt.Sprintf("upstream auth error %d: %s", code, snippet),
			code,
			"upstream_auth_error",
			nil,
			nil,
		)
	default:
		return transport.NewUpstreamError(
			fmt.Sprintf("upstream error code %d", code),
			502,
			"upstream_error",
			nil,
			nil,
		)
	}
}
