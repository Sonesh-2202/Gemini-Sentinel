package toolbridge

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	fencedBlockRe         = regexp.MustCompile("(?is)(?:```(?:json|tool_call|tool_calls|function_call|function_calls)?\\s*([\\s\\S]*?)```|<(?:tool_call|tool_calls|function_call|function_calls)>\\s*([\\s\\S]*?)</(?:tool_call|tool_calls|function_call|function_calls)>)")
	standaloneAfterRe     = regexp.MustCompile("(?is)^(?:</(?:tool_call|tool_calls|function_call|function_calls)>|```)\\s*$")
	standaloneBeforeRe    = regexp.MustCompile("(?is)(?:^|\\n)\\s*(?:<(?:tool_call|tool_calls|function_call|function_calls)>|```(?:json|tool_call|tool_calls|function_call|function_calls)?)\\s*$")
	exampleIntroRe        = regexp.MustCompile(`(?is)(?:\bdo\s+not\s+execute\b|\bdon't\s+execute\b|\bfor\s+example\b|\be\.g\.\b|\bexample\s*(?:json|call|payload|envelope|output|response|format)?\s*:\s*|\bformat\s+is\s*:\s*)\s*$`)
	potentialToolMarkerRe = regexp.MustCompile("(?is)(?:\"\\s*(?:tool_calls|toolcalls|tool_call|function_call|function_calls|functioncalls)\\s*\"|<(?:tool_call|tool_calls|function_call|function_calls)\\b|```\\s*(?:json|tool_call|tool_calls|function_call|function_calls)\\b|\\{\\s*\"\\s*(?:name|tool|tool_name|function|tool_calls|toolcalls|tool_call|function_call|function_calls|functioncalls)\\s*\"\\s*:|\\[\\s*\\{\\s*\"\\s*(?:name|tool|tool_name|function)\\s*\"\\s*:)")
	truncatedEnvRe        = regexp.MustCompile("(?is)(?:(?:^|[\\r\\n]\\s*|```(?:json|tool_call|tool_calls|function_call|function_calls)?\\s*|<(?:tool_call|tool_calls|function_call|function_calls)>\\s*)\\{\\s*\"\\s*(?:tool_calls|toolCalls|tool_call|function_call|function_calls|functionCalls)\\s*\"\\s*:|(?:```(?:json|tool_call|tool_calls|function_call|function_calls)|<(?:tool_call|tool_calls|function_call|function_calls)>)\\s*\\{\\s*\"\\s*(?:name|tool|tool_name)\\s*\"\\s*:)")
	gapFenceRe            = regexp.MustCompile("(?is)^(?:\\s|```(?:json|tool_call|tool_calls|function_call|function_calls)?|</?(?:tool_call|tool_calls|function_call|function_calls)>)*$")
	trailingTagRe         = regexp.MustCompile("(?is)(?:```(?:json|tool_call|tool_calls|function_call|function_calls)?|</?(?:tool_call|tool_calls|function_call|function_calls)>)\\s*$")
	trailingEnvRe         = regexp.MustCompile(`(?s)\{\s*"\s*(?:tool_calls|toolCalls|tool_call|function_call|function_calls|functionCalls)\s*"[\s\S]*$`)
	sanitizedPipeRe       = regexp.MustCompile(`<([A-Za-z_][A-Za-z0-9_]*)\s+([A-Za-z_][A-Za-z0-9_]*)\s+\|>`)
	htmlTagRe             = regexp.MustCompile(`<([A-Za-z][A-Za-z0-9:_-]*\s[^>\n]*?)>`)
)

type candidateSpan struct {
	raw    string
	start  int
	end    int
	whole  bool
	fenced bool
}

func findBalancedObjects(s string) [][2]int {
	var spans [][2]int
	depth := 0
	start := -1
	inStr := false
	esc := false
	for i := range len(s) {
		ch := s[i]
		if inStr {
			if esc {
				esc = false
			} else if ch == '\\' {
				esc = true
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
			if depth == 0 {
				start = i
			}
			depth++
		} else if ch == '}' {
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					spans = append(spans, [2]int{start, i + 1})
					start = -1
				}
			}
		}
	}
	return spans
}

func repairJSONStringEscapes(s string) string {
	if s == "" {
		return s
	}
	var out strings.Builder
	out.Grow(len(s) + 16)
	inStr := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !inStr {
			if ch == '"' {
				inStr = true
				out.WriteByte(ch)
				continue
			}
			// Strip trailing commas before '}' or ']' outside string literals.
			if ch == ',' {
				j := i + 1
				for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r' || s[j] == '\n') {
					j++
				}
				if j < len(s) && (s[j] == '}' || s[j] == ']') {
					continue
				}
			}
			out.WriteByte(ch)
			continue
		}
		if ch == '"' {
			inStr = false
			out.WriteByte(ch)
			continue
		}
		switch ch {
		case '\n':
			out.WriteString(`\n`)
			continue
		case '\r':
			out.WriteString(`\r`)
			continue
		case '\t':
			out.WriteString(`\t`)
			continue
		case '\\':
			if i+1 < len(s) {
				next := s[i+1]
				switch next {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
					out.WriteByte(ch)
					out.WriteByte(next)
					i++
					continue
				case 'u':
					if i+6 <= len(s) && isHex4(s[i+2:i+6]) {
						out.WriteString(s[i : i+6])
						i += 5
						continue
					}
				}
			}
			out.WriteString(`\\`)
			continue
		}
		out.WriteByte(ch)
	}
	return out.String()
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := range 4 {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func repairSanitizedHTMLInJSON(s string) string {
	if s == "" || (!strings.Contains(s, "{") && !strings.Contains(s, "[")) {
		return s
	}
	out := repairJSONStringEscapes(s)
	if strings.Contains(out, "<") {
		out = sanitizedPipeRe.ReplaceAllString(out, `<$1 | $2>`)
		out = htmlTagRe.ReplaceAllStringFunc(out, func(full string) string {
			inner := full[1 : len(full)-1]
			if !strings.Contains(inner, `"`) {
				return full
			}
			inner = strings.ReplaceAll(inner, `""`, `\"`)
			var fixed strings.Builder
			for i := range len(inner) {
				if inner[i] == '"' && (i == 0 || inner[i-1] != '\\') {
					fixed.WriteString(`\"`)
				} else {
					fixed.WriteByte(inner[i])
				}
			}
			return "<" + fixed.String() + ">"
		})
	}
	return out
}

func isStandaloneSpan(text string, start, end int) bool {
	before := text[:start]
	after := text[end:]

	if exampleIntroRe.MatchString(before) {
		return false
	}

	lineStart := strings.LastIndexByte(before, '\n') + 1
	beforeOnLine := strings.TrimSpace(before[lineStart:])
	if beforeOnLine != "" && !standaloneBeforeRe.MatchString(before) {
		return false
	}

	afterOnLine := after
	if nl := strings.IndexByte(after, '\n'); nl >= 0 {
		afterOnLine = after[:nl]
	}
	afterOnLine = strings.TrimSpace(afterOnLine)
	if afterOnLine != "" && !standaloneAfterRe.MatchString(afterOnLine) {
		return false
	}

	afterTrimmed := strings.TrimSpace(after)
	if afterTrimmed == "" || standaloneAfterRe.MatchString(afterTrimmed) {
		return true
	}
	return strings.HasPrefix(after, "\n") || strings.HasPrefix(after, "\r\n")
}

func trimMapKeysRecursive(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, elem := range val {
			cleaned := trimMapKeysRecursive(elem)
			tk := strings.TrimSpace(k)
			if tk != "" && tk != k {
				if _, exists := val[tk]; !exists {
					out[tk] = cleaned
					continue
				}
			}
			out[k] = cleaned
		}
		return out
	case []any:
		for i, elem := range val {
			val[i] = trimMapKeysRecursive(elem)
		}
		return val
	default:
		return v
	}
}

func unmarshalCandidateObject(raw string) (map[string]any, bool) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil && parsed != nil {
		if strings.Contains(raw, " |>") {
			if rep := repairSanitizedHTMLInJSON(raw); rep != raw {
				var repParsed map[string]any
				if err2 := json.Unmarshal([]byte(rep), &repParsed); err2 == nil && repParsed != nil {
					return trimMapKeysRecursive(repParsed).(map[string]any), true
				}
			}
		}
		return trimMapKeysRecursive(parsed).(map[string]any), true
	}
	if rep := repairSanitizedHTMLInJSON(raw); rep != raw {
		if err := json.Unmarshal([]byte(rep), &parsed); err == nil && parsed != nil {
			return trimMapKeysRecursive(parsed).(map[string]any), true
		}
	}
	return nil, false
}

func unmarshalCandidateArray(raw string) ([]any, bool) {
	var arr []any
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return trimMapKeysRecursive(arr).([]any), true
	}
	if rep := repairSanitizedHTMLInJSON(raw); rep != raw {
		if err := json.Unmarshal([]byte(rep), &arr); err == nil && len(arr) > 0 {
			return trimMapKeysRecursive(arr).([]any), true
		}
	}
	return nil, false
}

func callCandidates(text string) []candidateSpan {
	trimmed := strings.TrimSpace(text)
	var out []candidateSpan
	if trimmed == "" {
		return out
	}

	out = append(out, candidateSpan{raw: trimmed, start: 0, end: len(trimmed), whole: true, fenced: false})
	for _, loc := range fencedBlockRe.FindAllStringSubmatchIndex(trimmed, -1) {
		if exampleIntroRe.MatchString(trimmed[:loc[0]]) {
			continue
		}
		subStart, subEnd := -1, -1
		if len(loc) >= 4 && loc[2] >= 0 && loc[3] >= 0 {
			subStart, subEnd = loc[2], loc[3]
		} else if len(loc) >= 6 && loc[4] >= 0 && loc[5] >= 0 {
			subStart, subEnd = loc[4], loc[5]
		}
		if subStart >= 0 && subEnd >= subStart {
			raw := strings.TrimSpace(trimmed[subStart:subEnd])
			if raw != "" {
				out = append(out, candidateSpan{raw: raw, start: loc[0], end: loc[1], whole: false, fenced: true})
			}
		}
	}
	for _, sp := range findBalancedObjects(trimmed) {
		out = append(out, candidateSpan{
			raw:    trimmed[sp[0]:sp[1]],
			start:  sp[0],
			end:    sp[1],
			whole:  false,
			fenced: isStandaloneSpan(trimmed, sp[0], sp[1]),
		})
	}
	return out
}

func looksLikeEnvelope(v map[string]any) bool {
	if v == nil {
		return false
	}
	for k := range v {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "tool_calls", "toolcalls", "tool_call", "function_call", "function_calls", "functioncalls":
			return true
		}
	}
	return false
}

func extractBareCallName(v map[string]any) string {
	if v == nil {
		return ""
	}
	for _, wrapKey := range []string{"function", "tool_call", "function_call", "call", "tool", "invoke"} {
		if sub, ok := v[wrapKey].(map[string]any); ok && sub != nil {
			if n := extractBareCallName(sub); n != "" {
				return n
			}
		}
	}
	for _, k := range []string{"name", "tool", "tool_name", "toolName", "function", "function_name", "functionName", "action", "recipient_name"} {
		if n, ok := v[k].(string); ok && strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n)
		}
	}
	for k, rawVal := range v {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "name", "tool", "tool_name", "toolname", "function", "function_name", "functionname", "action", "recipient_name":
			if n, ok := rawVal.(string); ok && strings.TrimSpace(n) != "" {
				return strings.TrimSpace(n)
			}
		}
	}
	return ""
}

func looksLikeBareCall(v map[string]any) bool {
	if extractBareCallName(v) == "" {
		return false
	}
	target := v
	if sub, ok := v["function"].(map[string]any); ok && sub != nil {
		target = sub
	}
	for _, k := range []string{"arguments", "parameters", "args", "input", "params", "action_input"} {
		if _, ok := target[k]; ok {
			return true
		}
		if _, ok := v[k]; ok {
			return true
		}
	}
	return false
}

var reservedCallEnvelopeKeys = map[string]bool{
	"name": true, "tool": true, "tool_name": true, "toolname": true,
	"function": true, "function_name": true, "functionname": true,
	"action": true, "recipient_name": true, "type": true,
	"id": true, "call_id": true, "tool_call_id": true, "index": true,
	"arguments": true, "parameters": true, "args": true, "input": true, "params": true, "action_input": true,
}

func resolveAllowedToolName(name string, allow map[string]map[string]any) string {
	name = strings.TrimSpace(name)
	if name == "" || allow == nil {
		return name
	}
	if _, exists := allow[name]; exists {
		return name
	}
	for _, sep := range []string{":", ".", "/"} {
		if idx := strings.LastIndex(name, sep); idx >= 0 && idx+1 < len(name) {
			short := name[idx+1:]
			if _, exists := allow[short]; exists {
				return short
			}
		}
	}
	for k := range allow {
		if strings.EqualFold(k, name) {
			return k
		}
	}
	return name
}

func inferToolNameFromSchema(candArgs map[string]any, allow map[string]map[string]any, prevName string) string {
	if len(candArgs) == 0 || len(allow) == 0 {
		return ""
	}
	var matches []string
	bestExtraProps := int(^uint(0) >> 1)
	for tName, schema := range allow {
		if schema == nil {
			continue
		}
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			continue
		}
		allKnown := true
		for k := range candArgs {
			if _, exists := props[k]; !exists {
				allKnown = false
				break
			}
		}
		if !allKnown {
			continue
		}
		if CheckSchema(schema, candArgs, "arguments") != "" {
			continue
		}
		extra := len(props) - len(candArgs)
		if extra < bestExtraProps {
			bestExtraProps = extra
			matches = []string{tName}
		} else if extra == bestExtraProps {
			matches = append(matches, tName)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	if len(matches) > 1 && prevName != "" {
		for _, m := range matches {
			if m == prevName {
				return prevName
			}
		}
	}
	return ""
}

func flattenCallSlice(raw any) []any {
	s, ok := raw.([]any)
	if !ok {
		return []any{raw}
	}
	var out []any
	for _, elem := range s {
		if nested, isSlice := elem.([]any); isSlice {
			out = append(out, flattenCallSlice(nested)...)
		} else {
			out = append(out, elem)
		}
	}
	return out
}

func normaliseCalls(envelope map[string]any, allow map[string]map[string]any) ([]Call, error) {
	var raw any
	for _, k := range []string{"tool_calls", "toolCalls", "tool_call", "function_call", "function_calls", "functionCalls"} {
		if v, ok := envelope[k]; ok {
			raw = v
			break
		}
	}
	if raw == nil {
		for k, v := range envelope {
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "tool_calls", "toolcalls", "tool_call", "function_call", "function_calls", "functioncalls":
				raw = v
			}
			if raw != nil {
				break
			}
		}
	}
	if raw == nil {
		return nil, nil
	}
	rawSlice := flattenCallSlice(raw)
	if len(rawSlice) == 0 {
		return nil, nil
	}

	out := make([]Call, 0, len(rawSlice))
	for _, itemAny := range rawSlice {
		if itemAny == nil {
			continue
		}
		itemMap, ok := itemAny.(map[string]any)
		if !ok || itemMap == nil {
			return nil, toolCallError("a tool call was missing its function name")
		}
		if len(itemMap) == 0 {
			continue
		}
		for _, wrapKey := range []string{"tool_call", "function_call", "call", "invoke"} {
			if sub, isMap := itemMap[wrapKey].(map[string]any); isMap && len(sub) > 0 {
				itemMap = sub
				break
			}
		}
		fn := itemMap
		if sub, ok := itemMap["function"].(map[string]any); ok && sub != nil {
			fn = sub
		}
		name := resolveAllowedToolName(extractBareCallName(itemMap), allow)

		var argsRaw any
		for _, k := range []string{"arguments", "parameters", "args", "input", "params", "action_input"} {
			if val, exists := fn[k]; exists {
				argsRaw = val
				break
			}
			if val, exists := itemMap[k]; exists {
				argsRaw = val
				break
			}
		}

		if name == "" && allow != nil {
			// Check tool-name-as-key pattern: {"write": {"path": "...", "content": "..."}}
			for k, v := range itemMap {
				resolved := resolveAllowedToolName(k, allow)
				if _, isAllowed := allow[resolved]; isAllowed {
					if _, isMap := v.(map[string]any); isMap {
						name = resolved
						argsRaw = v
						break
					}
				}
			}
		}
		if name == "" && allow != nil {
			prevName := ""
			if len(out) > 0 {
				prevName = out[len(out)-1].Name
			}
			if argMap, isMap := argsRaw.(map[string]any); isMap {
				name = inferToolNameFromSchema(argMap, allow, prevName)
			} else if argStr, isStr := argsRaw.(string); isStr {
				if parsedArgs, ok := unmarshalCandidateObject(argStr); ok {
					name = inferToolNameFromSchema(parsedArgs, allow, prevName)
				}
			} else if argsRaw == nil {
				cleanItem := make(map[string]any, len(itemMap))
				for k, v := range itemMap {
					if !reservedCallEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))] {
						cleanItem[k] = v
					}
				}
				if len(cleanItem) == 0 && len(fn) > 0 {
					for k, v := range fn {
						if !reservedCallEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))] {
							cleanItem[k] = v
						}
					}
				}
				if inferred := inferToolNameFromSchema(cleanItem, allow, prevName); inferred != "" {
					name = inferred
					argsRaw = cleanItem
				}
			}
		}
		if name == "" {
			return nil, toolCallError("a tool call was missing its function name")
		}
		if allow != nil {
			if _, exists := allow[name]; !exists {
				return nil, toolCallError(fmt.Sprintf(`the model called %q, which is not among the tools of this request`, name))
			}
		}

		var args map[string]any
		switch a := argsRaw.(type) {
		case nil:
			args = map[string]any{}
			for k, v := range fn {
				if !reservedCallEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))] {
					args[k] = v
				}
			}
			if len(args) == 0 && len(itemMap) > 0 {
				for k, v := range itemMap {
					if !reservedCallEnvelopeKeys[strings.ToLower(strings.TrimSpace(k))] {
						args[k] = v
					}
				}
			}
		case string:
			var parsed any
			if err := json.Unmarshal([]byte(a), &parsed); err != nil {
				if rep := repairSanitizedHTMLInJSON(a); rep != a {
					err = json.Unmarshal([]byte(rep), &parsed)
				}
				if err != nil {
					return nil, toolCallError(fmt.Sprintf(`arguments for %q were not valid JSON`, name))
				}
			}
			parsed = trimMapKeysRecursive(parsed)
			m, ok := parsed.(map[string]any)
			if !ok || m == nil {
				return nil, toolCallError(fmt.Sprintf(`arguments for %q must be a JSON object`, name))
			}
			args = m
		case map[string]any:
			args = make(map[string]any, len(a))
			for k, v := range a {
				args[k] = v
			}
		default:
			return nil, toolCallError(fmt.Sprintf(`arguments for %q must be a JSON object`, name))
		}
		var schema map[string]any
		if allow != nil {
			schema = allow[name]
		}
		if schema != nil {
			reqSlice := extractStringSlice(schema["required"])
			reqSet := make(map[string]bool, len(reqSlice))
			for _, r := range reqSlice {
				reqSet[r] = true
			}
			props, _ := schema["properties"].(map[string]any)
			for k, v := range args {
				if v == nil {
					propSchema := any(nil)
					if props != nil {
						propSchema = props[k]
					}
					propMap, _ := propSchema.(map[string]any)
					isNullable := propMap != nil && (propMap["nullable"] == true || CheckSchema(propSchema, nil, k) == "")
					if !reqSet[k] || isNullable {
						if !reqSet[k] {
							delete(args, k)
						}
					}
				}
			}
			for _, reqKey := range reqSlice {
				if _, exists := args[reqKey]; !exists && props != nil {
					if propMap, ok := props[reqKey].(map[string]any); ok {
						if defVal, hasDef := propMap["default"]; hasDef && defVal != nil {
							args[reqKey] = defVal
						}
					}
				}
			}
		}

		for k, v := range args {
			if s, ok := v.(string); ok && strings.Contains(s, " |>") {
				args[k] = sanitizedPipeRe.ReplaceAllString(s, `<$1 | $2>`)
			}
		}

		if errStr := CheckSchema(schema, args, "arguments"); errStr != "" {
			return nil, toolCallError(fmt.Sprintf(`arguments for %q violate the tool schema: %s`, name, errStr))
		}
		out = append(out, Call{Name: name, Arguments: args})
	}
	return out, nil
}

// ParseToolReply extracts and validates tool calls from a model reply.
func ParseToolReply(reply string, tools any) (*ParseResult, error) {
	trimmed := strings.TrimSpace(reply)
	var allow map[string]map[string]any
	if tools != nil {
		contract, err := ValidateToolRequest(tools, "auto")
		if err != nil {
			return nil, err
		}
		if len(contract.Tools) > 0 {
			allow = make(map[string]map[string]any, len(contract.Tools))
			for _, t := range contract.Tools {
				allow[t.Name] = t.Parameters
			}
		}
	}
	if trimmed == "" {
		return &ParseResult{Reason: "empty reply", Text: ""}, nil
	}

	type foundEnv struct {
		value map[string]any
		span  candidateSpan
	}
	var envelopes []foundEnv
	var bares []foundEnv

	for _, cand := range callCandidates(reply) {
		if parsed, ok := unmarshalCandidateObject(cand.raw); ok {
			if looksLikeEnvelope(parsed) {
				if cand.whole || cand.fenced {
					envelopes = append(envelopes, foundEnv{value: parsed, span: cand})
				}
				continue
			}
			if looksLikeBareCall(parsed) && (cand.whole || cand.fenced) {
				name := extractBareCallName(parsed)
				if allow == nil {
					bares = append(bares, foundEnv{value: map[string]any{"tool_calls": []any{parsed}}, span: cand})
				} else if _, ok := allow[name]; ok {
					bares = append(bares, foundEnv{value: map[string]any{"tool_calls": []any{parsed}}, span: cand})
				}
			}
			continue
		}
		// Also support a top-level JSON array of tool call objects: [{"name": "read", "arguments": {...}}]
		if (cand.whole || cand.fenced) && strings.HasPrefix(strings.TrimSpace(cand.raw), "[") {
			if arr, ok := unmarshalCandidateArray(cand.raw); ok {
				allCalls := true
				for _, elem := range arr {
					m, ok := elem.(map[string]any)
					if !ok || !looksLikeBareCall(m) {
						allCalls = false
						break
					}
					if allow != nil {
						if _, ok := allow[extractBareCallName(m)]; !ok {
							allCalls = false
							break
						}
					}
				}
				if allCalls {
					envelopes = append(envelopes, foundEnv{value: map[string]any{"tool_calls": arr}, span: cand})
				}
			}
		}
	}

	activeList := envelopes
	if len(activeList) == 0 && len(bares) > 0 {
		activeList = bares
	}
	var (
		found    *foundEnv
		foundIdx int
		calls    []Call
	)
	for idx := range activeList {
		c, cErr := normaliseCalls(activeList[idx].value, allow)
		if cErr != nil {
			return nil, cErr
		}
		if len(c) > 0 {
			found = &activeList[idx]
			foundIdx = idx
			calls = c
			break
		}
		if found == nil {
			found = &activeList[idx]
			foundIdx = idx
		}
	}
	if found == nil {
		if truncatedEnvRe.MatchString(trimmed) {
			return nil, toolCallError("the model attempted a tool_calls JSON envelope that was malformed or truncated")
		}
		return &ParseResult{Reason: "no tool call envelope in the reply", Text: trimmed}, nil
	}

	if len(calls) > 0 && len(activeList) > foundIdx+1 {
		prevEnd := found.span.end
		for i := foundIdx + 1; i < len(activeList); i++ {
			next := activeList[i]
			if next.span.start < prevEnd {
				continue
			}
			if prevEnd <= len(trimmed) && next.span.start <= len(trimmed) {
				gap := trimmed[prevEnd:next.span.start]
				if !gapFenceRe.MatchString(gap) {
					break
				}
			}
			extra, extraErr := normaliseCalls(next.value, allow)
			if extraErr != nil {
				return nil, extraErr
			}
			if len(extra) > 0 {
				calls = append(calls, extra...)
				prevEnd = next.span.end
			}
		}
	}
	if len(calls) == 0 {
		return &ParseResult{Reason: "tool call envelope without calls", Text: trimmed}, nil
	}

	cutPos := found.span.start
	if cutPos > len(trimmed) {
		cutPos = len(trimmed)
	}
	before := strings.TrimSpace(trailingTagRe.ReplaceAllString(trimmed[:cutPos], ""))
	before = strings.TrimSpace(trailingEnvRe.ReplaceAllString(before, ""))

	return &ParseResult{
		Calls: calls,
		Text:  before,
	}, nil
}

// InterpretReply decides whether a model reply should be returned as tool_calls.
func InterpretReply(reply string, tools any, toolChoice any) (*InterpretResult, error) {
	contract, err := ValidateToolRequest(tools, toolChoice)
	if err != nil {
		return nil, err
	}
	if len(contract.Tools) == 0 || contract.ChoiceMode == "none" {
		return nil, nil
	}
	parsed, err := ParseToolReply(reply, contract.Tools)
	if err != nil {
		return nil, err
	}
	if len(parsed.Calls) > 0 {
		if contract.ChoiceMode == "function" && contract.ForcedFunction != "" {
			for _, c := range parsed.Calls {
				if c.Name != contract.ForcedFunction {
					return nil, toolCallError(fmt.Sprintf(`the request required %q but the reply also called %q`, contract.ForcedFunction, c.Name))
				}
			}
		}
		return &InterpretResult{Calls: parsed.Calls, Text: parsed.Text}, nil
	}
	if contract.ChoiceMode == "required" || contract.ChoiceMode == "function" {
		return nil, toolCallError(fmt.Sprintf("the model did not return the required tool call (%s)", parsed.Reason))
	}
	return nil, nil
}

// ExtractPreToolPreamble returns any text preceding a tool_calls JSON envelope
// and its opening markdown fence or XML tag.
func ExtractPreToolPreamble(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	loc := trailingEnvRe.FindStringIndex(trimmed)
	if len(loc) != 2 {
		return ""
	}
	before := strings.TrimSpace(trailingTagRe.ReplaceAllString(trimmed[:loc[0]], ""))
	before = strings.TrimSpace(trailingEnvRe.ReplaceAllString(before, ""))
	return before
}

// SafeStreamablePrefix returns a safe prefix of text that can be streamed live
// before generation completes when tools are enabled, holding back a 140-byte
// lookahead guard and refusing to stream if text starts with or contains any
// tool-call envelope/fence markers.
func SafeStreamablePrefix(text string) string {
	if len(text) <= 320 {
		return ""
	}
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if trimmed == "" || trimmed[0] == '{' || trimmed[0] == '[' || trimmed[0] == '<' || strings.HasPrefix(trimmed, "```") {
		return ""
	}
	safeEnd := len(text) - 140
	for i := range len(text) {
		ch := text[i]
		if ch == '{' || ch == '[' || ch == '<' || (ch == '`' && strings.HasPrefix(text[i:], "```")) {
			if i < safeEnd {
				safeEnd = i
			}
			break
		}
	}
	for safeEnd > 0 && !utf8.RuneStart(text[safeEnd]) {
		safeEnd--
	}
	if safeEnd <= 0 {
		return ""
	}
	if potentialToolMarkerRe.MatchString(text) {
		return ""
	}
	return text[:safeEnd]
}
