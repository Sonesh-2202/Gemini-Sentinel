package toolbridge

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

var callSeq atomic.Uint64

func nextToolCallID() string {
	seq := callSeq.Add(1) % 1_000_000
	ts := strconv.FormatInt(time.Now().UnixMilli(), 36)
	seqStr := strconv.FormatUint(seq, 36)
	for len(seqStr) < 3 {
		seqStr = "0" + seqStr
	}
	return "call_" + ts + seqStr
}

// ToOpenAIToolCalls formats normalized Calls into OpenAI tool_calls objects.
func ToOpenAIToolCalls(calls []Call) []OpenAIToolCall {
	out := make([]OpenAIToolCall, 0, len(calls))
	for _, c := range calls {
		argsMap := c.Arguments
		if argsMap == nil {
			argsMap = map[string]any{}
		}
		b, _ := json.Marshal(argsMap)
		out = append(out, OpenAIToolCall{
			ID:   nextToolCallID(),
			Type: "function",
			Function: OpenAIFunctionCall{
				Name:      c.Name,
				Arguments: string(b),
			},
		})
	}
	return out
}

// StreamOpenAIToolCallChunks emits the 2-step OpenAI streaming tool_calls delta sequence
// for pre-constructed OpenAIToolCall items so IDs remain identical across cache and SSE.
func StreamOpenAIToolCallChunks(emit func(delta map[string]any), oa []OpenAIToolCall) {
	for idx, call := range oa {
		emit(map[string]any{
			"tool_calls": []map[string]any{
				{
					"index": idx,
					"id":    call.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      call.Function.Name,
						"arguments": "",
					},
				},
			},
		})
		emitArgumentStringChunks(emit, idx, call.Function.Arguments)
	}
}

func emitArgumentStringChunks(emit func(delta map[string]any), idx int, args string) {
	const maxChunk = 4096
	if len(args) <= maxChunk {
		emit(map[string]any{
			"tool_calls": []map[string]any{
				{
					"index": idx,
					"function": map[string]any{
						"arguments": args,
					},
				},
			},
		})
		return
	}
	for start := 0; start < len(args); {
		end := start + maxChunk
		if end >= len(args) {
			end = len(args)
		} else {
			for end > start && !utf8.RuneStart(args[end]) {
				end--
			}
		}
		emit(map[string]any{
			"tool_calls": []map[string]any{
				{
					"index": idx,
					"function": map[string]any{
						"arguments": args[start:end],
					},
				},
			},
		})
		start = end
	}
}

type streamedCallState struct {
	id         string
	name       string
	headerSent bool
	sentArgs   string
	finalArgs  string
	done       bool
}

// LiveToolCallStreamer incrementally emits OpenAI tool_calls delta chunks as Gemini Web
// generates a {"tool_calls":[...]} JSON envelope, while guaranteeing schema validity.
type LiveToolCallStreamer struct {
	contract *Contract
	allow    map[string]map[string]any
	emit     func(delta map[string]any)
	states   []streamedCallState
	aborted  bool
}

// NewLiveToolCallStreamer constructs an incremental tool_calls streamer for one attempt.
func NewLiveToolCallStreamer(tools any, toolChoice any, emit func(delta map[string]any)) *LiveToolCallStreamer {
	contract, err := ValidateToolRequest(tools, toolChoice)
	if err != nil || len(contract.Tools) == 0 || contract.ChoiceMode == "none" {
		return &LiveToolCallStreamer{aborted: true, emit: emit}
	}
	allow := make(map[string]map[string]any, len(contract.Tools))
	for _, t := range contract.Tools {
		allow[t.Name] = t.Parameters
	}
	return &LiveToolCallStreamer{
		contract: contract,
		allow:    allow,
		emit:     emit,
	}
}

// SentAny reports whether at least one tool_calls chunk has been emitted.
func (s *LiveToolCallStreamer) SentAny() bool {
	for _, st := range s.states {
		if st.headerSent || st.sentArgs != "" {
			return true
		}
	}
	return false
}

func (s *LiveToolCallStreamer) ensureState(idx int, name string) *streamedCallState {
	for len(s.states) <= idx {
		s.states = append(s.states, streamedCallState{
			id: nextToolCallID(),
		})
	}
	if s.states[idx].name == "" {
		s.states[idx].name = name
	}
	return &s.states[idx]
}

// Push inspects the cumulative candidate text and emits any newly available tool call
// headers or verified argument slices.
func (s *LiveToolCallStreamer) Push(text string) {
	if s == nil || s.aborted || s.emit == nil {
		return
	}
	arrStart := findToolCallsArrayStart(text)
	if arrStart < 0 {
		return
	}
	pos := arrStart
	for idx := 0; ; idx++ {
		pos = skipJSONWSAndCommas(text, pos)
		if pos >= len(text) || text[pos] == ']' {
			return
		}
		if text[pos] != '{' {
			s.aborted = true
			return
		}
		objEnd := findSingleBalancedObjectEnd(text, pos)
		if objEnd > pos {
			if idx < len(s.states) && s.states[idx].done {
				pos = objEnd
				continue
			}
			rawObj := text[pos:objEnd]
			parsed, ok := unmarshalCandidateObject(rawObj)
			if !ok {
				s.aborted = true
				return
			}
			normCalls, err := normaliseCalls(map[string]any{"tool_calls": []any{parsed}}, s.allow)
			if err != nil || len(normCalls) != 1 {
				s.aborted = true
				return
			}
			call := normCalls[0]
			if s.contract.ChoiceMode == "function" && s.contract.ForcedFunction != "" && idx == 0 && call.Name != s.contract.ForcedFunction {
				s.aborted = true
				return
			}
			st := s.ensureState(idx, call.Name)
			st.name = call.Name
			if !st.headerSent {
				st.headerSent = true
				s.emit(map[string]any{
					"tool_calls": []map[string]any{
						{
							"index": idx,
							"id":    st.id,
							"type":  "function",
							"function": map[string]any{
								"name":      st.name,
								"arguments": "",
							},
						},
					},
				})
			}
			if st.sentArgs != "" {
				rawArgs := extractRawArgumentsObject(rawObj)
				if strings.HasPrefix(rawArgs, st.sentArgs) {
					tail := rawArgs[len(st.sentArgs):]
					if strings.Contains(tail, " |>") || !json.Valid([]byte(st.sentArgs+tail)) {
						tail = repairSanitizedHTMLInJSON(tail)
					}
					if tail != "" {
						emitArgumentStringChunks(s.emit, idx, tail)
					}
					st.finalArgs = st.sentArgs + tail
					st.done = true
					pos = objEnd
					continue
				}
			}
			b, _ := json.Marshal(call.Arguments)
			fullArgs := string(b)
			emitArgumentStringChunks(s.emit, idx, fullArgs)
			st.finalArgs = fullArgs
			st.done = true
			pos = objEnd
			continue
		}

		// Incomplete object at end of text: check if we can emit its header and verified partial arguments
		partialObj := text[pos:]
		name, argsOffset, ok := parsePartialCallPrefix(partialObj)
		if !ok {
			return
		}
		if _, allowed := s.allow[name]; !allowed {
			return
		}
		if s.contract.ChoiceMode == "function" && s.contract.ForcedFunction != "" && idx == 0 && name != s.contract.ForcedFunction {
			return
		}
		partialArgs := partialObj[argsOffset:]
		headerOK, safeLen := verifyPartialArgsForLiveStream(partialArgs, s.allow[name])
		if !headerOK {
			return
		}
		st := s.ensureState(idx, name)
		if !st.headerSent {
			st.headerSent = true
			s.emit(map[string]any{
				"tool_calls": []map[string]any{
					{
						"index": idx,
						"id":    st.id,
						"type":  "function",
						"function": map[string]any{
							"name":      st.name,
							"arguments": "",
						},
					},
				},
			})
		}
		if safeLen > len(st.sentArgs) && strings.HasPrefix(partialArgs[:safeLen], st.sentArgs) {
			delta := partialArgs[len(st.sentArgs):safeLen]
			st.sentArgs = partialArgs[:safeLen]
			emitArgumentStringChunks(s.emit, idx, delta)
		}
		return
	}
}

// Finalize completes emission of all validated tool calls at the end of a turn and returns
// the canonical OpenAIToolCall slice with matching IDs and argument strings.
func (s *LiveToolCallStreamer) Finalize(calls []Call, fullText string) []OpenAIToolCall {
	if s != nil && !s.aborted && fullText != "" {
		s.Push(fullText)
	}
	out := make([]OpenAIToolCall, 0, len(calls))
	for idx, c := range calls {
		argsMap := c.Arguments
		if argsMap == nil {
			argsMap = map[string]any{}
		}
		b, _ := json.Marshal(argsMap)
		argsStr := string(b)
		callID := ""
		if s != nil && idx < len(s.states) {
			st := &s.states[idx]
			callID = st.id
			if st.done && st.finalArgs != "" {
				out = append(out, OpenAIToolCall{
					ID:   callID,
					Type: "function",
					Function: OpenAIFunctionCall{
						Name:      c.Name,
						Arguments: st.finalArgs,
					},
				})
				continue
			}
			if s.emit != nil {
				if !st.headerSent {
					st.headerSent = true
					s.emit(map[string]any{
						"tool_calls": []map[string]any{
							{
								"index": idx,
								"id":    callID,
								"type":  "function",
								"function": map[string]any{
									"name":      c.Name,
									"arguments": "",
								},
							},
						},
					})
				}
				if st.sentArgs != "" && strings.HasPrefix(argsStr, st.sentArgs) {
					if tail := argsStr[len(st.sentArgs):]; tail != "" {
						emitArgumentStringChunks(s.emit, idx, tail)
					}
				} else if st.sentArgs == "" {
					emitArgumentStringChunks(s.emit, idx, argsStr)
				}
				st.done = true
				st.finalArgs = argsStr
			}
		}
		if callID == "" {
			callID = nextToolCallID()
			if s != nil && s.emit != nil {
				s.emit(map[string]any{
					"tool_calls": []map[string]any{
						{
							"index": idx,
							"id":    callID,
							"type":  "function",
							"function": map[string]any{
								"name":      c.Name,
								"arguments": "",
							},
						},
					},
				})
				emitArgumentStringChunks(s.emit, idx, argsStr)
			}
		}
		out = append(out, OpenAIToolCall{
			ID:   callID,
			Type: "function",
			Function: OpenAIFunctionCall{
				Name:      c.Name,
				Arguments: argsStr,
			},
		})
	}
	return out
}

func findToolCallsArrayStart(s string) int {
	idx := strings.Index(s, `"tool_calls"`)
	if idx < 0 {
		return -1
	}
	prev := idx - 1
	for prev >= 0 && (s[prev] == ' ' || s[prev] == '\t' || s[prev] == '\r' || s[prev] == '\n') {
		prev--
	}
	if prev < 0 || (s[prev] != '{' && s[prev] != ',') {
		return -1
	}
	pos := idx + len(`"tool_calls"`)
	pos = skipJSONWS(s, pos)
	if pos >= len(s) || s[pos] != ':' {
		return -1
	}
	pos = skipJSONWS(s, pos+1)
	if pos >= len(s) || s[pos] != '[' {
		return -1
	}
	return pos + 1
}

func skipJSONWS(s string, pos int) int {
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\r' || s[pos] == '\n') {
		pos++
	}
	return pos
}

func skipJSONWSAndCommas(s string, pos int) int {
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\r' || s[pos] == '\n' || s[pos] == ',') {
		pos++
	}
	return pos
}

func findSingleBalancedObjectEnd(s string, start int) int {
	if start >= len(s) || s[start] != '{' {
		return -1
	}
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inStr {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		if ch == '"' {
			inStr = true
			continue
		}
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

func parseJSONStringLiteral(s string, start int) (val string, next int, ok bool) {
	if start >= len(s) || s[start] != '"' {
		return "", start, false
	}
	escaped := false
	for i := start + 1; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			var out string
			if err := json.Unmarshal([]byte(s[start:i+1]), &out); err != nil {
				return "", start, false
			}
			return out, i + 1, true
		}
	}
	return "", start, false
}

func parsePartialCallPrefix(partialObj string) (name string, argsBraceOffset int, ok bool) {
	pos := skipJSONWS(partialObj, 0)
	if pos >= len(partialObj) || partialObj[pos] != '{' {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, pos+1)
	k1, next, ok := parseJSONStringLiteral(partialObj, pos)
	if !ok || k1 != "name" {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, next)
	if pos >= len(partialObj) || partialObj[pos] != ':' {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, pos+1)
	toolName, next, ok := parseJSONStringLiteral(partialObj, pos)
	if !ok || toolName == "" {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, next)
	if pos >= len(partialObj) || partialObj[pos] != ',' {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, pos+1)
	k2, next, ok := parseJSONStringLiteral(partialObj, pos)
	if !ok || k2 != "arguments" {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, next)
	if pos >= len(partialObj) || partialObj[pos] != ':' {
		return "", 0, false
	}
	pos = skipJSONWS(partialObj, pos+1)
	if pos >= len(partialObj) || partialObj[pos] != '{' {
		return "", 0, false
	}
	return toolName, pos, true
}

func extractRawArgumentsObject(rawObj string) string {
	_, argsOffset, ok := parsePartialCallPrefix(rawObj)
	if !ok {
		return ""
	}
	end := findSingleBalancedObjectEnd(rawObj, argsOffset)
	if end <= argsOffset {
		return ""
	}
	return rawObj[argsOffset:end]
}

func verifyPartialArgsForLiveStream(partialArgs string, schema map[string]any) (headerOK bool, safeLen int) {
	if len(partialArgs) < 2 || partialArgs[0] != '{' {
		return false, 0
	}
	if strings.Contains(partialArgs, " |>") {
		return false, 0
	}
	props, _ := schema["properties"].(map[string]any)
	reqList := extractStringSlice(schema["required"])

	seenKeys := make(map[string]bool)
	depth := 0
	inStr := false
	escaped := false
	expectingKey := false
	for i := 0; i < len(partialArgs); i++ {
		ch := partialArgs[i]
		if inStr {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inStr = false
			}
			continue
		}
		switch ch {
		case '{':
			depth++
			if depth == 1 {
				expectingKey = true
			}
		case '}':
			depth--
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 1 {
				expectingKey = true
			}
		case '"':
			if depth == 1 && expectingKey {
				key, next, ok := parseJSONStringLiteral(partialArgs, i)
				if !ok {
					// Key string literal still being generated
					i = len(partialArgs)
					break
				}
				afterKey := skipJSONWS(partialArgs, next)
				if afterKey >= len(partialArgs) {
					i = len(partialArgs)
					break
				}
				if partialArgs[afterKey] != ':' {
					return false, 0
				}
				valPos := skipJSONWS(partialArgs, afterKey+1)
				if valPos >= len(partialArgs) {
					i = len(partialArgs)
					break
				}
				if props != nil {
					propSchema, exists := props[key]
					if !exists {
						return false, 0
					}
					if !isValueTokenCompatibleWithSchema(partialArgs[valPos], propSchema) {
						return false, 0
					}
				}
				seenKeys[key] = true
				expectingKey = false
				i = valPos - 1
				continue
			}
			inStr = true
		}
	}
	if len(seenKeys) == 0 && len(reqList) > 0 {
		return false, 0
	}
	headerOK = true
	if len(partialArgs) < 120 {
		return true, 0
	}
	for _, req := range reqList {
		if !seenKeys[req] {
			if props != nil {
				if pm, ok := props[req].(map[string]any); ok && pm["default"] != nil {
					continue
				}
			}
			return true, 0
		}
	}
	cut := len(partialArgs) - 8
	for cut > 0 && !utf8.RuneStart(partialArgs[cut]) {
		cut--
	}
	for back := 1; back <= 6 && cut-back >= 0; back++ {
		if partialArgs[cut-back] == '\\' {
			cut -= back
			break
		}
	}
	if cut <= 1 {
		return true, 0
	}
	if repairJSONStringEscapes(partialArgs[:cut]) != partialArgs[:cut] {
		return true, 0
	}
	return true, cut
}

func isValueTokenCompatibleWithSchema(firstByte byte, propSchema any) bool {
	pm, ok := propSchema.(map[string]any)
	if !ok || pm == nil {
		return firstByte != 'n'
	}
	tStr, _ := pm["type"].(string)
	if tStr == "" {
		return firstByte != 'n'
	}
	switch tStr {
	case "string":
		return firstByte == '"'
	case "object":
		return firstByte == '{'
	case "array":
		return firstByte == '['
	case "boolean":
		return firstByte == 't' || firstByte == 'f'
	case "number", "integer":
		return firstByte == '-' || (firstByte >= '0' && firstByte <= '9')
	default:
		return true
	}
}
