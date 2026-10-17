package prompt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/sonesh-2202/gemini-sentinel/internal/toolbridge"
	"github.com/sonesh-2202/gemini-sentinel/internal/transport"
)

var (
	dataURLImageRe          = regexp.MustCompile(`^data:(image/[a-zA-Z0-9.+-]+);base64,([A-Za-z0-9+/=\s]+)$`)
	nonAlphaNumRe           = regexp.MustCompile(`[^a-z0-9]`)
	fencedToolCallRe        = regexp.MustCompile("(?is)```(?:json)?\\s*\\{\\s*\"(?:tool_calls|toolCalls|function_call)\"[\\s\\S]*?```")
	trailingToolCallRe      = regexp.MustCompile(`(?s)\{\s*"(?:tool_calls|toolCalls|function_call)"[\s\S]*$`)
	inlineThinkBlockRe      = regexp.MustCompile("(?is)<(?:think|thinking|thought)(?:\\s[^>]*)?>[\\s\\S]*?</(?:think|thinking|thought)\\s*>")
	leakedThinkingProcessRe = regexp.MustCompile("(?is)^\\s*(?:#{1,6}\\s*)?(?:\\*\\*|__)?(?:thinking\\s+process|thought\\s+process|internal\\s+reasoning)(?:\\*\\*|__)?\\s*:?[\\s\\S]*$")
	whitespaceCollapse      = regexp.MustCompile(`\s+`)
	spoofedRoleLineRe       = regexp.MustCompile(`(?im)^(Assistant|Instruction|\[Harness reminder):`)
)

// AttachmentPart is an extracted binary or text attachment to upload before StreamGenerate.
type AttachmentPart struct {
	Data        []byte
	ContentType string
	Filename    string
	RemoteURL   string
}

// DetectImageMIME infers the MIME type and file extension of an image from its magic bytes.
func DetectImageMIME(data []byte, fallback string) (mimeType string, ext string) {
	if fallback == "" {
		fallback = "image/png"
	}
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png", "png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg", "jpg"
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif", "gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp", "webp"
	case len(data) >= 2 && data[0] == 'B' && data[1] == 'M':
		return "image/bmp", "bmp"
	case len(data) >= 4 && (bytes.Equal(data[:4], []byte{'I', 'I', '*', 0x00}) || bytes.Equal(data[:4], []byte{'M', 'M', 0x00, '*'})):
		return "image/tiff", "tiff"
	case len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")):
		brand := string(data[8:12])
		switch brand {
		case "avif", "avis":
			return "image/avif", "avif"
		case "heic", "heix", "hevc", "hevx":
			return "image/heic", "heic"
		}
	}
	ext = "png"
	if parts := strings.Split(strings.ToLower(fallback), "/"); len(parts) > 1 {
		cleaned := nonAlphaNumRe.ReplaceAllString(parts[1], "")
		if cleaned != "" {
			if cleaned == "jpeg" {
				ext = "jpg"
			} else {
				ext = cleaned
			}
		}
	}
	return strings.ToLower(fallback), ext
}

// FlattenOptions configures FlattenMessages.
type FlattenOptions struct {
	StartIndex      int
	MaxPromptChars  int
	MaxOldToolChars int
	AllowOverflow   bool
}

// ParseDataURLImage decodes a data:image/...;base64,... URL into an AttachmentPart.
func ParseDataURLImage(rawURL string, index int) *AttachmentPart {
	m := dataURLImageRe.FindStringSubmatch(strings.TrimSpace(rawURL))
	if len(m) < 3 {
		return nil
	}
	b64Clean := whitespaceCollapse.ReplaceAllString(m[2], "")
	data, err := base64.StdEncoding.DecodeString(b64Clean)
	if err != nil || len(data) == 0 {
		return nil
	}
	contentType, ext := DetectImageMIME(data, strings.ToLower(m[1]))
	return &AttachmentPart{
		Data:        data,
		ContentType: contentType,
		Filename:    fmt.Sprintf("image_%d.%s", index+1, ext),
	}
}

func extractSingleImagePart(part map[string]any, index int) *AttachmentPart {
	if part == nil {
		return nil
	}
	fromURL := func(rawURL, fallbackMIME string) *AttachmentPart {
		u := strings.TrimSpace(rawURL)
		if u == "" {
			return nil
		}
		if strings.HasPrefix(strings.ToLower(u), "data:") {
			return ParseDataURLImage(u, index)
		}
		if strings.HasPrefix(strings.ToLower(u), "https://") || strings.HasPrefix(strings.ToLower(u), "http://") {
			mime, ext := DetectImageMIME(nil, fallbackMIME)
			return &AttachmentPart{
				RemoteURL:   u,
				ContentType: mime,
				Filename:    fmt.Sprintf("image_%d.%s", index+1, ext),
			}
		}
		return nil
	}

	pType, _ := part["type"].(string)
	switch pType {
	case "image_url", "input_image":
		var rawURL, mime string
		if s, ok := part["image_url"].(string); ok {
			rawURL = s
		} else if sub, ok := part["image_url"].(map[string]any); ok {
			rawURL, _ = sub["url"].(string)
			mime, _ = sub["mime_type"].(string)
		}
		if rawURL == "" {
			rawURL, _ = part["url"].(string)
		}
		if mime == "" {
			mime, _ = part["mime_type"].(string)
		}
		return fromURL(rawURL, mime)
	case "image":
		var rawURL string
		if s, ok := part["image_url"].(string); ok {
			rawURL = s
		} else if sub, ok := part["image_url"].(map[string]any); ok {
			rawURL, _ = sub["url"].(string)
		}
		if rawURL == "" {
			rawURL, _ = part["url"].(string)
		}
		mime, _ := part["mediaType"].(string)
		if mime == "" {
			mime, _ = part["mimeType"].(string)
		}
		if mime == "" {
			mime, _ = part["mime_type"].(string)
		}
		if rawURL != "" {
			return fromURL(rawURL, mime)
		}
		if dataStr, ok := part["data"].(string); ok && dataStr != "" {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(dataStr)), "data:") {
				return fromURL(dataStr, mime)
			}
			if mime == "" {
				mime = "image/png"
			}
			return fromURL(fmt.Sprintf("data:%s;base64,%s", mime, dataStr), mime)
		}
	}
	return nil
}

// ExtractImageParts extracts all inline base64 or remote http(s) images from OpenAI/Anthropic message arrays.
func ExtractImageParts(messages []map[string]any) []AttachmentPart {
	var images []AttachmentPart
	for _, m := range messages {
		if m == nil {
			continue
		}
		contentArr, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, partAny := range contentArr {
			part, ok := partAny.(map[string]any)
			if !ok || part == nil {
				continue
			}
			if att := extractSingleImagePart(part, len(images)); att != nil {
				images = append(images, *att)
			}
		}
	}
	return images
}

// blockedImageIP reports whether an IP must never be contacted for remote
// image fetching: loopback, private, link-local, multicast, unspecified,
// reserved, and carrier-grade NAT ranges (SSRF / metadata-service guard).
func blockedImageIP(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return true
	}
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return true
	}
	if addr.Is4() {
		b := addr.As4()
		// 0.0.0.0/8, 100.64.0.0/10 (CGNAT), 169.254.0.0/16, 192.0.0.0/24,
		// 192.0.2.0/24 (TEST-NET-1), 198.18.0.0/15 (benchmark), 198.51.100.0/24,
		// 203.0.113.0/24, 224.0.0.0/4, 240.0.0.0/4.
		if b[0] == 0 || b[0] >= 224 {
			return true
		}
		if b[0] == 100 && b[1] >= 64 && b[1] < 128 {
			return true
		}
		if b[0] == 169 && b[1] == 254 {
			return true
		}
		if b[0] == 192 && b[1] == 0 && (b[2] == 0 || b[2] == 2) {
			return true
		}
		if b[0] == 198 && (b[1] == 18 || b[1] == 19) {
			return true
		}
		if b[0] == 198 && b[1] == 51 && b[2] == 100 {
			return true
		}
		if b[0] == 203 && b[1] == 0 && b[2] == 113 {
			return true
		}
		if b[0] == 192 && b[1] == 88 && b[2] == 99 {
			return true
		}
	}
	// Reserved / non-global IPv6 (documentation, discard, etc.).
	if !addr.IsGlobalUnicast() {
		return true
	}
	return false
}

// resolveAndValidateImageHost resolves host and rejects it when any returned
// address is blocked. This runs before dialing and on every redirect hop to
// narrow DNS-rebinding windows; the dialer Control below re-validates the
// actually-dialed address as the final guard.
func resolveAndValidateImageHost(ctx context.Context, host string) error {
	bare := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		bare = h
	}
	if addr, err := netip.ParseAddr(strings.Trim(bare, "[]")); err == nil {
		if blockedImageIP(addr) {
			return fmt.Errorf("blocked image address %s", addr.String())
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", bare)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("cannot resolve image host %s", bare)
	}
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice([]byte(ip.To16())); ok {
			if blockedImageIP(a) {
				return fmt.Errorf("blocked image host %s", bare)
			}
		} else if addr, err := netip.ParseAddr(ip.String()); err == nil {
			if blockedImageIP(addr) {
				return fmt.Errorf("blocked image host %s", bare)
			}
		}
	}
	return nil
}

// FetchRemoteImage downloads an http(s) image URL with SSRF protection, a 20MB cap,
// and magic-byte MIME detection.
func FetchRemoteImage(ctx context.Context, rawURL string, index int) (*AttachmentPart, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, transport.NewUpstreamError("invalid remote image URL", 400, "invalid_request_error", nil, err)
	}
	allowPrivate := os.Getenv("SENTINEL_ALLOW_PRIVATE_IMAGE_URLS") == "1"
	if !allowPrivate {
		if err := resolveAndValidateImageHost(ctx, parsed.Host); err != nil {
			return nil, transport.NewUpstreamError("blocked remote image host: "+err.Error(), 400, "invalid_request_error", nil, err)
		}
	}
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			if allowPrivate {
				return nil
			}
			host, _, splitErr := net.SplitHostPort(address)
			if splitErr != nil {
				host = address
			}
			addr, addrErr := netip.ParseAddr(host)
			if addrErr != nil {
				return fmt.Errorf("unresolved IP %s", host)
			}
			if blockedImageIP(addr.Unmap()) {
				return fmt.Errorf("blocked private or loopback image address %s", addr.String())
			}
			return nil
		},
	}
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			DialContext: dialer.DialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many image redirects")
			}
			if req.URL == nil || (req.URL.Scheme != "https" && req.URL.Scheme != "http") {
				return fmt.Errorf("unsupported redirect scheme")
			}
			if !allowPrivate && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing https to http redirect")
			}
			if !allowPrivate {
				if err := resolveAndValidateImageHost(req.Context(), req.URL.Host); err != nil {
					return err
				}
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, transport.NewUpstreamError("invalid remote image request", 400, "invalid_request_error", nil, err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, transport.NewUpstreamError("failed to fetch remote image: "+err.Error(), 400, "invalid_request_error", nil, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, transport.NewUpstreamError(fmt.Sprintf("remote image returned HTTP %d", resp.StatusCode), 400, "invalid_request_error", nil, nil)
	}
	const maxImageBytes = 20 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil || len(data) == 0 {
		return nil, transport.NewUpstreamError("remote image body is empty or unreadable", 400, "invalid_request_error", nil, err)
	}
	if len(data) > maxImageBytes {
		return nil, transport.NewUpstreamError("remote image exceeds 20MB limit", 400, "invalid_request_error", nil, nil)
	}
	headerMIME := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	mime, ext := DetectImageMIME(data, headerMIME)
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		return nil, transport.NewUpstreamError("remote image has unsupported content type", 400, "invalid_request_error", nil, nil)
	}
	if !isKnownImageMagic(data) && headerMIME != "" &&
		!strings.HasPrefix(strings.ToLower(headerMIME), "image/") &&
		strings.ToLower(headerMIME) != "application/octet-stream" {
		return nil, transport.NewUpstreamError("remote image has unsupported content type", 400, "invalid_request_error", nil, nil)
	}
	if index < 0 {
		index = 0
	}
	return &AttachmentPart{
		Data:        data,
		ContentType: mime,
		Filename:    fmt.Sprintf("image_%d.%s", index+1, ext),
	}, nil
}

// isKnownImageMagic reports whether data starts with a recognized image signature.
func isKnownImageMagic(data []byte) bool {
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return true
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return true
	}
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		return true
	}
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return true
	}
	if len(data) >= 2 && data[0] == 'B' && data[1] == 'M' {
		return true
	}
	return false
}

// BoundOldToolText middle-truncates text exceeding cap, preserving 65% head and 35% tail.
func BoundOldToolText(text string, cap int, label string) string {
	if cap <= 0 || len(text) <= cap {
		return text
	}
	if label == "" {
		label = "earlier tool output"
	}
	head := int(float64(cap) * 0.65)
	tail := cap - head
	omitted := len(text) - head - tail
	return fmt.Sprintf("%s\n...[truncated %d chars of %s]...\n%s", text[:head], omitted, label, text[len(text)-tail:])
}

type turnEntry struct {
	role         string
	text         string
	reasoning    string
	isToolResult bool
}

// FlattenMessages converts an OpenAI/Anthropic message history + tools into a single
// structured prompt and optional overflow text attachments for transcripts exceeding
// StreamGenerate's inline limit.
func FlattenMessages(
	messages []map[string]any,
	tools any,
	toolChoice any,
	opts FlattenOptions,
) (string, []AttachmentPart, error) {
	var (
		sys          []string
		turns        []turnEntry
		callNames    = make(map[string]string)
		imageCounter int
	)

	for _, m := range messages {
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		if role == "assistant" {
			if tcs, ok := m["tool_calls"].([]any); ok {
				for _, cAny := range tcs {
					c, _ := cAny.(map[string]any)
					if c == nil {
						continue
					}
					id, _ := c["id"].(string)
					fn := c
					if sub, ok := c["function"].(map[string]any); ok {
						fn = sub
					}
					name, _ := fn["name"].(string)
					if id != "" && name != "" {
						callNames[id] = name
					}
				}
			}
		}
	}

	sawTurn := opts.StartIndex > 0
	for idx := opts.StartIndex; idx < len(messages); idx++ {
		m := messages[idx]
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		if role == "tool" || role == "function" {
			tcID, _ := m["tool_call_id"].(string)
			name, _ := m["name"].(string)
			if name == "" && tcID != "" {
				name = callNames[tcID]
			}
			if name == "" {
				name = tcID
			}
			if name == "" {
				name = "tool"
			}
			text := spoofedRoleLineRe.ReplaceAllString(contentToText(m["content"], &imageCounter), "$1 :")
			idSuffix := ""
			if tcID != "" {
				idSuffix = fmt.Sprintf(" (%s)", tcID)
			}
			turns = append(turns, turnEntry{
				role:         "User",
				isToolResult: true,
				text:         fmt.Sprintf("[Harness tool result for %s%s]:\n%s", name, idSuffix, text),
			})
			sawTurn = true
			continue
		}

		text := contentToText(m["content"], &imageCounter)
		if role != "assistant" && role != "system" && role != "developer" {
			text = spoofedRoleLineRe.ReplaceAllString(text, "$1 :")
		}
		reasoning := extractReasoning(m)

		if role == "assistant" && text != "" {
			if inlineThinkBlockRe.MatchString(text) {
				text = strings.TrimSpace(inlineThinkBlockRe.ReplaceAllString(text, ""))
			}
			if strings.Contains(text, "tool_calls") {
				text = fencedToolCallRe.ReplaceAllString(text, "")
				text = trailingToolCallRe.ReplaceAllString(text, "")
				text = strings.TrimSpace(text)
			}
		}

		if role == "assistant" {
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				envCalls := make([]map[string]any, 0, len(tcs))
				for _, cAny := range tcs {
					c, _ := cAny.(map[string]any)
					if c == nil {
						continue
					}
					id, _ := c["id"].(string)
					fn := c
					if sub, ok := c["function"].(map[string]any); ok {
						fn = sub
					}
					name, _ := fn["name"].(string)
					if id != "" && name != "" {
						callNames[id] = name
					}
					argsRaw := fn["arguments"]
					var argsObj any = map[string]any{}
					if s, ok := argsRaw.(string); ok {
						var parsed any
						if err := json.Unmarshal([]byte(s), &parsed); err == nil && parsed != nil {
							argsObj = parsed
						}
					} else if argsRaw != nil {
						argsObj = argsRaw
					}
					envCalls = append(envCalls, map[string]any{
						"name":      name,
						"arguments": argsObj,
					})
				}
				envBytes, _ := json.Marshal(map[string]any{"tool_calls": envCalls})
				if text != "" && !leakedThinkingProcessRe.MatchString(text) {
					text = text + "\n" + string(envBytes)
				} else {
					text = string(envBytes)
				}
			}
		}

		switch role {
		case "system", "developer":
			if !sawTurn {
				sys = append(sys, text)
			} else {
				turns = append(turns, turnEntry{role: "Instruction", text: text})
			}
		case "assistant":
			turns = append(turns, turnEntry{role: "Assistant", text: text, reasoning: reasoning})
			sawTurn = true
		default:
			turns = append(turns, turnEntry{role: "User", text: text})
			sawTurn = true
		}
	}

	contract, err := toolbridge.ValidateToolRequest(tools, toolChoice)
	if err != nil {
		return "", nil, err
	}
	hasTools := len(contract.Tools) > 0 && contract.ChoiceMode != "none"

	if hasTools && opts.StartIndex == 0 {
		instr, err := toolbridge.BuildToolInstructions(tools, toolChoice)
		if err != nil {
			return "", nil, err
		}
		if instr != "" {
			sys = append(sys, instr)
		}
	}

	header := ""
	if len(sys) > 0 {
		header = strings.Join(sys, "\n\n") + "\n\n"
	}

	reminder := ""
	if hasTools {
		reminder, _ = toolbridge.BuildToolReminder(tools, toolChoice)
	}
	footer := ""
	if reminder != "" {
		footer = reminder + "\n\n"
	}
	if len(turns) > 1 || hasTools {
		footer += "Assistant:"
	}

	turnBudget := 0
	if opts.MaxPromptChars > 0 {
		turnBudget = opts.MaxPromptChars - len(header) - len(footer)
		if turnBudget < 16000 {
			turnBudget = 16000
		}
	}

	var overflowFiles []AttachmentPart
	var body string

	if len(turns) > 1 {
		var assistantIndices []int
		for i := range turns {
			if turns[i].role == "Assistant" {
				assistantIndices = append(assistantIndices, i)
			}
		}
		recentCutoff := len(turns) - 6
		if recentCutoff < 0 {
			recentCutoff = 0
		}
		if len(assistantIndices) >= 2 {
			recentCutoff = assistantIndices[len(assistantIndices)-2]
		}
		_ = assistantIndices

		buildTurnStrings := func(toolCap, assistantCap, recentToolCap int) ([]string, int) {
			rendered := make([]string, len(turns))
			totalBytes := 0
			for i := range turns {
				t := turns[i]
				bodyText := t.text
				if i < recentCutoff {
					if t.isToolResult {
						bodyText = BoundOldToolText(bodyText, toolCap, "earlier tool output")
					} else if t.role == "Assistant" && assistantCap > 0 {
						bodyText = BoundOldToolText(bodyText, assistantCap, "earlier assistant turn")
					}
				} else if t.isToolResult && recentToolCap > 0 {
					bodyText = BoundOldToolText(bodyText, recentToolCap, "tool output")
				}
				_ = t.reasoning
				entry := fmt.Sprintf("%s: %s\n\n", t.role, bodyText)
				rendered[i] = entry
				totalBytes += len(entry)
			}
			return rendered, totalBytes
		}

		rendered, totalBytes := buildTurnStrings(opts.MaxOldToolChars, 0, 0)

		if turnBudget > 0 && totalBytes > turnBudget && opts.AllowOverflow && recentCutoff > 1 {
			const maxAttachmentBytes = 340000
			const maxAttachments = 9
			inlineTarget := turnBudget - 150000
			if inlineTarget < 200000 {
				inlineTarget = 200000
			}
			var chunks []string
			var curParts []string
			curBytes := 0
			offloadedTurns := 0

			for i := 1; i < recentCutoff && totalBytes > inlineTarget; i++ {
				entry := rendered[i]
				entryBytes := len(entry)
				if curBytes > 0 && curBytes+entryBytes > maxAttachmentBytes {
					if len(chunks)+1 >= maxAttachments {
						break
					}
					chunks = append(chunks, strings.Join(curParts, ""))
					curParts = nil
					curBytes = 0
				}
				curParts = append(curParts, entry)
				curBytes += entryBytes
				totalBytes -= entryBytes
				rendered[i] = ""
				offloadedTurns++
			}
			if len(curParts) > 0 {
				chunks = append(chunks, strings.Join(curParts, ""))
			}
			if len(chunks) > 0 {
				names := make([]string, 0, len(chunks))
				for cIdx := range chunks {
					filename := fmt.Sprintf("conversation_history_part_%d.txt", cIdx+1)
					names = append(names, filename)
					overflowFiles = append(overflowFiles, AttachmentPart{
						Data:        []byte(chunks[cIdx]),
						ContentType: "text/plain",
						Filename:    filename,
					})
				}
				note := fmt.Sprintf("Instruction: [Earlier conversation history (%d turns) is attached verbatim in %s. Read the attached file(s) as the chronological conversation history preceding the turns below.]\n\n", offloadedTurns, strings.Join(names, ", "))
				rendered[1] = note
				totalBytes += len(note)
			}
		}

		hasOffloaded := len(overflowFiles) > 0
		if !hasOffloaded && turnBudget > 0 && totalBytes > turnBudget {
			for _, capVal := range []int{16000, 8000, 4000, 2000, 1000, 500} {
				if opts.MaxOldToolChars > 0 && capVal >= opts.MaxOldToolChars {
					continue
				}
				rendered, totalBytes = buildTurnStrings(capVal, 0, 0)
				if totalBytes <= turnBudget {
					break
				}
			}
		}
		if !hasOffloaded && turnBudget > 0 && totalBytes > turnBudget {
			for _, aCap := range []int{6000, 3000, 1500} {
				rendered, totalBytes = buildTurnStrings(500, aCap, 0)
				if totalBytes <= turnBudget {
					break
				}
			}
		}

		elideOldTurns := func() {
			if !(recentCutoff > 1 && totalBytes > turnBudget) {
				return
			}
			dropped := 0
			for i := 1; i < recentCutoff && totalBytes > turnBudget-120; i++ {
				if rendered[i] == "" || (i == 1 && strings.HasPrefix(rendered[1], "Instruction: [")) {
					continue
				}
				totalBytes -= len(rendered[i])
				rendered[i] = ""
				dropped++
			}
			if dropped > 0 && !hasOffloaded {
				note := fmt.Sprintf("Instruction: [...omitted %d earlier turns to fit upstream 1M char limit...]\n\n", dropped)
				rendered[1] = note
				totalBytes += len(note)
			}
		}

		if turnBudget > 0 && totalBytes > turnBudget {
			elideOldTurns()
		}
		if !hasOffloaded && turnBudget > 0 && totalBytes > turnBudget {
			for _, rCap := range []int{64000, 32000, 16000, 8000} {
				rendered, totalBytes = buildTurnStrings(500, 1500, rCap)
				elideOldTurns()
				if totalBytes <= turnBudget {
					break
				}
			}
		}
		if turnBudget > 0 && totalBytes > turnBudget {
			var activeIndices []int
			for i := range rendered {
				if rendered[i] != "" && !(i == 1 && strings.HasPrefix(rendered[1], "Instruction: [")) {
					activeIndices = append(activeIndices, i)
				}
			}
			denom := len(activeIndices)
			if denom < 1 {
				denom = 1
			}
			perTurnCap := (turnBudget - 256) / denom
			if perTurnCap < 1000 {
				perTurnCap = 1000
			}
			for _, i := range activeIndices {
				if totalBytes <= turnBudget {
					break
				}
				oldLen := len(rendered[i])
				if oldLen > perTurnCap {
					t := turns[i]
					innerCap := perTurnCap - len(t.role) - 16
					if innerCap < 500 {
						innerCap = 500
					}
					clamped := fmt.Sprintf("%s: %s\n\n", t.role, BoundOldToolText(t.text, innerCap, "turn"))
					rendered[i] = clamped
					totalBytes = totalBytes - oldLen + len(clamped)
				}
			}
		}
		body = strings.Join(rendered, "")
	} else if len(turns) == 1 {
		singleText := turns[0].text
		if turnBudget > 0 {
			capVal := turnBudget - 32
			if capVal < 4000 {
				capVal = 4000
			}
			singleText = BoundOldToolText(singleText, capVal, "message")
		}
		if hasTools {
			body = fmt.Sprintf("User: %s\n\n", singleText)
		} else {
			body = singleText
		}
	}

	return header + body + footer, overflowFiles, nil
}

// EstimateTokens estimates token count at ~4 UTF-8 bytes per token.
func EstimateTokens(s string) int {
	n := len(s)
	if n == 0 {
		return 0
	}
	t := (n + 3) / 4
	if t < 1 {
		return 1
	}
	return t
}

// BuildUsage constructs the OpenAI usage object including reasoning_tokens details.
func BuildUsage(promptText, completionText, reasoningText string) map[string]any {
	promptTokens := EstimateTokens(promptText)
	textTokens := EstimateTokens(completionText)
	reasoningTokens := EstimateTokens(reasoningText)
	completionTokens := textTokens + reasoningTokens
	usage := map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	}
	if reasoningTokens > 0 {
		usage["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": reasoningTokens,
		}
	}
	return usage
}

// HistoryHasher pre-normalizes and pre-marshals message entries and tool definitions
// once in O(N) so prefix hashes for any (accountID, modelID, split) can be computed
// in O(1) without repeated JSON marshaling or base64 image decoding.
type HistoryHasher struct {
	joinedMsgs []byte
	endOffsets []int
	footer     []byte
}

// NewHistoryHasher constructs a reusable O(N) prefix hasher for a conversation history.
func NewHistoryHasher(messages []map[string]any, tools any, toolChoice any) *HistoryHasher {
	endOffsets := make([]int, len(messages)+1)
	var joined bytes.Buffer
	for i, m := range messages {
		if i > 0 {
			joined.WriteByte(',')
		}
		entryBytes, _ := json.Marshal(normalizeMessageForHash(m))
		joined.Write(entryBytes)
		endOffsets[i+1] = joined.Len()
	}

	var footer bytes.Buffer
	footer.WriteByte(']')
	if tools != nil || toolChoice != nil {
		tb, _ := json.Marshal(tools)
		cb, _ := json.Marshal(toolChoice)
		footer.WriteByte(',')
		footer.Write(tb)
		footer.WriteByte(',')
		footer.Write(cb)
	}
	footer.WriteByte(']')

	return &HistoryHasher{
		joinedMsgs: joined.Bytes(),
		endOffsets: endOffsets,
		footer:     footer.Bytes(),
	}
}

// Len returns the number of messages indexed in the hasher.
func (h *HistoryHasher) Len() int {
	if h == nil || len(h.endOffsets) == 0 {
		return 0
	}
	return len(h.endOffsets) - 1
}

// HashPrefix returns the SHA-256 hex digest for messages[:split] on (accountID, modelID).
func (h *HistoryHasher) HashPrefix(accountID, modelID string, split int) string {
	if h == nil {
		return ""
	}
	if split < 0 {
		split = 0
	} else if split >= len(h.endOffsets) {
		split = len(h.endOffsets) - 1
	}
	ab, _ := json.Marshal(accountID)
	mb, _ := json.Marshal(modelID)
	hasher := sha256.New()
	hasher.Write([]byte{'['})
	hasher.Write(ab)
	hasher.Write([]byte{','})
	hasher.Write(mb)
	hasher.Write([]byte{',', '['})
	hasher.Write(h.joinedMsgs[:h.endOffsets[split]])
	hasher.Write(h.footer)
	var sum [sha256.Size]byte
	return hex.EncodeToString(hasher.Sum(sum[:0]))
}

// HashHistoryWithTools computes a deterministic SHA-256 hash over (accountID, modelID, messages, tools, toolChoice)
// so changes to images, tool_call_id, assistant text, or tool definitions never collide in SessionCache.
func HashHistoryWithTools(accountID, modelID string, messages []map[string]any, tools any, toolChoice any) string {
	return NewHistoryHasher(messages, tools, toolChoice).HashPrefix(accountID, modelID, len(messages))
}

func normalizeMessageForHash(m map[string]any) any {
	if m == nil {
		return nil
	}
	role, _ := m["role"].(string)
	if role == "developer" {
		role = "system"
	}
	var text string
	var imgDigests []string
	if s, ok := m["content"].(string); ok {
		text = s
	} else if arr, ok := m["content"].([]any); ok {
		var parts []string
		for _, pAny := range arr {
			p, _ := pAny.(map[string]any)
			if p == nil {
				continue
			}
			pType, _ := p["type"].(string)
			if pType == "text" || pType == "input_text" {
				t, _ := p["text"].(string)
				parts = append(parts, t)
			} else if att := extractSingleImagePart(p, len(imgDigests)); att != nil {
				if att.RemoteURL != "" {
					imgDigests = append(imgDigests, "url:"+att.RemoteURL)
				} else if len(att.Data) > 0 {
					sum := sha256.Sum256(att.Data)
					imgDigests = append(imgDigests, "sha256:"+hex.EncodeToString(sum[:]))
				}
			}
		}
		text = strings.Join(parts, "\n")
	}

	var normTools []map[string]any
	if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
		for _, tcAny := range tcs {
			tc, _ := tcAny.(map[string]any)
			if tc == nil {
				continue
			}
			tcID, _ := tc["id"].(string)
			fn := tc
			if sub, ok := tc["function"].(map[string]any); ok {
				fn = sub
			}
			name, _ := fn["name"].(string)
			var args any = map[string]any{}
			if s, ok := fn["arguments"].(string); ok {
				var parsed any
				if err := json.Unmarshal([]byte(s), &parsed); err == nil && parsed != nil {
					args = parsed
				}
			} else if fn["arguments"] != nil {
				args = fn["arguments"]
			}
			normTools = append(normTools, map[string]any{"id": tcID, "name": name, "args": args})
		}
	}

	entry := map[string]any{
		"role": role,
		"text": text,
	}
	if role == "tool" || role == "function" {
		if tcID, _ := m["tool_call_id"].(string); tcID != "" {
			entry["tool_call_id"] = tcID
		}
		if name, _ := m["name"].(string); name != "" {
			entry["name"] = name
		}
	}
	if len(normTools) > 0 {
		entry["tools"] = normTools
	}
	if len(imgDigests) > 0 {
		entry["images"] = imgDigests
	}
	return entry
}

func contentToText(content any, imageCounter *int) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			if s, ok := item.(string); ok {
				if s != "" {
					parts = append(parts, s)
				}
			} else if m, ok := item.(map[string]any); ok && m != nil {
				if t, ok := m["text"].(string); ok && t != "" {
					parts = append(parts, t)
				} else if extractSingleImagePart(m, *imageCounter) != nil {
					*imageCounter++
					parts = append(parts, fmt.Sprintf("[Attached image #%d]", *imageCounter))
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		b, _ := json.Marshal(c)
		return string(b)
	}
}

func extractReasoning(m map[string]any) string {
	if s, ok := m["reasoning_content"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if s, ok := m["thinking"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if arr, ok := m["content"].([]any); ok {
		var parts []string
		for _, item := range arr {
			if sub, ok := item.(map[string]any); ok && sub["type"] == "thinking" {
				if t, ok := sub["thinking"].(string); ok && strings.TrimSpace(t) != "" {
					parts = append(parts, strings.TrimSpace(t))
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
