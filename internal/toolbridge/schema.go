package toolbridge

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// CheckSchema validates value against a JSON Schema subset matching tool-bridge.js.
func CheckSchema(schema any, value any, path string) string {
	var root map[string]any
	if sMap, ok := schema.(map[string]any); ok {
		root = sMap
	}
	return checkSchemaWithRoot(schema, root, value, path, 0)
}

func resolveSchemaRef(root map[string]any, ref string) (any, bool) {
	if root == nil || !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
	var cur any = root
	for _, p := range parts {
		p = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok || m == nil {
			return nil, false
		}
		next, exists := m[p]
		if !exists {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func checkSchemaWithRoot(schema any, root map[string]any, value any, path string, depth int) string {
	if depth > 64 || schema == nil || schema == true {
		return ""
	}
	if b, ok := schema.(bool); ok && !b {
		return fmt.Sprintf("%s is not allowed", path)
	}
	sMap, ok := schema.(map[string]any)
	if !ok {
		return fmt.Sprintf("%s has an invalid schema definition", path)
	}
	if root == nil {
		root = sMap
	}

	if refStr, ok := sMap["$ref"].(string); ok && strings.TrimSpace(refStr) != "" {
		target, resolved := resolveSchemaRef(root, strings.TrimSpace(refStr))
		if !resolved {
			return fmt.Sprintf("%s failed unresolved schema $ref %q", path, refStr)
		}
		if errStr := checkSchemaWithRoot(target, root, value, path, depth+1); errStr != "" {
			return errStr
		}
	}

	if notSchema, exists := sMap["not"]; exists && notSchema != nil {
		if checkSchemaWithRoot(notSchema, root, value, path, depth+1) == "" {
			return fmt.Sprintf("%s matches forbidden not-schema", path)
		}
	}

	if value == nil {
		if nullable, _ := sMap["nullable"].(bool); nullable {
			return ""
		}
	}

	if tRaw, exists := sMap["type"]; exists && tRaw != nil {
		types := extractStringSlice(tRaw)
		if s, ok := tRaw.(string); ok {
			types = []string{s}
		}
		if len(types) > 0 {
			actual := jsonTypeOf(value)
			matched := false
			for _, t := range types {
				if t == actual || (t == "integer" && actual == "number" && isIntegerValue(value)) {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Sprintf("%s must be %s (got %s)", path, strings.Join(types, "|"), actual)
			}
		}
	}

	if enumRaw, ok := sMap["enum"].([]any); ok {
		matched := false
		for _, e := range enumRaw {
			if jsonEqual(e, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Sprintf("%s is not one of the allowed values", path)
		}
	}

	if constVal, exists := sMap["const"]; exists {
		if !jsonEqual(constVal, value) {
			return fmt.Sprintf("%s must equal the declared constant", path)
		}
	}

	if strVal, ok := value.(string); ok {
		runeLen := utf8.RuneCountInString(strVal)
		if minLen, ok := intFromAny(sMap["minLength"]); ok && runeLen < minLen {
			return fmt.Sprintf("%s is shorter than %d", path, minLen)
		}
		if maxLen, ok := intFromAny(sMap["maxLength"]); ok && runeLen > maxLen {
			return fmt.Sprintf("%s is longer than %d", path, maxLen)
		}
		if pat, ok := sMap["pattern"].(string); ok && pat != "" {
			re, err := regexp.Compile(pat)
			if err != nil {
				return fmt.Sprintf("%s has invalid schema pattern %q", path, pat)
			}
			if !re.MatchString(strVal) {
				return fmt.Sprintf("%s does not match the required pattern", path)
			}
		}
	}

	if numVal, ok := floatFromAny(value); ok {
		if minVal, ok := floatFromAny(sMap["minimum"]); ok && numVal < minVal {
			return fmt.Sprintf("%s is below %v", path, minVal)
		}
		if maxVal, ok := floatFromAny(sMap["maximum"]); ok && numVal > maxVal {
			return fmt.Sprintf("%s is above %v", path, maxVal)
		}
		if exMin, ok := floatFromAny(sMap["exclusiveMinimum"]); ok && numVal <= exMin {
			return fmt.Sprintf("%s must be above %v", path, exMin)
		}
		if exMax, ok := floatFromAny(sMap["exclusiveMaximum"]); ok && numVal >= exMax {
			return fmt.Sprintf("%s must be below %v", path, exMax)
		}
		if mult, ok := floatFromAny(sMap["multipleOf"]); ok && mult > 0 {
			q := numVal / mult
			if math.Abs(q-math.Round(q)) > 1e-9 {
				return fmt.Sprintf("%s is not a multiple of %v", path, mult)
			}
		}
	}

	if arrVal, ok := value.([]any); ok {
		if minItems, ok := intFromAny(sMap["minItems"]); ok && len(arrVal) < minItems {
			return fmt.Sprintf("%s has fewer than %d items", path, minItems)
		}
		if maxItems, ok := intFromAny(sMap["maxItems"]); ok && len(arrVal) > maxItems {
			return fmt.Sprintf("%s has more than %d items", path, maxItems)
		}
		if uniq, _ := sMap["uniqueItems"].(bool); uniq {
			for i := range arrVal {
				for j := i + 1; j < len(arrVal); j++ {
					if jsonEqual(arrVal[i], arrVal[j]) {
						return fmt.Sprintf("%s has duplicate items", path)
					}
				}
			}
		}
		if prefixItems, ok := sMap["prefixItems"].([]any); ok {
			for i := range arrVal {
				if i < len(prefixItems) {
					if errStr := checkSchemaWithRoot(prefixItems[i], root, arrVal[i], fmt.Sprintf("%s[%d]", path, i), depth+1); errStr != "" {
						return errStr
					}
				} else if addItems, exists := sMap["items"]; exists {
					if errStr := checkSchemaWithRoot(addItems, root, arrVal[i], fmt.Sprintf("%s[%d]", path, i), depth+1); errStr != "" {
						return errStr
					}
				}
			}
		} else if itemsRaw, exists := sMap["items"]; exists && itemsRaw != nil {
			if itemsArr, ok := itemsRaw.([]any); ok {
				for i := range arrVal {
					var subSchema any
					if i < len(itemsArr) {
						subSchema = itemsArr[i]
					} else if addItems, hasAdd := sMap["additionalItems"]; hasAdd {
						subSchema = addItems
					}
					if errStr := checkSchemaWithRoot(subSchema, root, arrVal[i], fmt.Sprintf("%s[%d]", path, i), depth+1); errStr != "" {
						return errStr
					}
				}
			} else {
				for i := range arrVal {
					if errStr := checkSchemaWithRoot(itemsRaw, root, arrVal[i], fmt.Sprintf("%s[%d]", path, i), depth+1); errStr != "" {
						return errStr
					}
				}
			}
		}
	}

	if objVal, ok := value.(map[string]any); ok && objVal != nil {
		for _, reqKey := range extractStringSlice(sMap["required"]) {
			if _, exists := objVal[reqKey]; !exists {
				return fmt.Sprintf(`%s is missing required property %q`, path, reqKey)
			}
		}
		props, _ := sMap["properties"].(map[string]any)
		if props != nil {
			for k, sub := range props {
				if v, exists := objVal[k]; exists {
					if errStr := checkSchemaWithRoot(sub, root, v, path+"."+k, depth+1); errStr != "" {
						return errStr
					}
				}
			}
		}
		if addProps, ok := sMap["additionalProperties"].(bool); ok && !addProps {
			for k := range objVal {
				if props == nil {
					return fmt.Sprintf(`%s has unexpected property %q`, path, k)
				}
				if _, declared := props[k]; !declared {
					return fmt.Sprintf(`%s has unexpected property %q`, path, k)
				}
			}
		} else if addPropSchema, ok := sMap["additionalProperties"].(map[string]any); ok {
			for k, v := range objVal {
				if props != nil {
					if _, declared := props[k]; declared {
						continue
					}
				}
				if errStr := checkSchemaWithRoot(addPropSchema, root, v, path+"."+k, depth+1); errStr != "" {
					return errStr
				}
			}
		}
	}

	if allOf, ok := sMap["allOf"].([]any); ok {
		for _, sub := range allOf {
			if errStr := checkSchemaWithRoot(sub, root, value, path, depth+1); errStr != "" {
				return errStr
			}
		}
	}

	for _, kw := range []string{"anyOf", "oneOf"} {
		if branches, ok := sMap[kw].([]any); ok && len(branches) > 0 {
			matches := 0
			for _, sub := range branches {
				if checkSchemaWithRoot(sub, root, value, path, depth+1) == "" {
					matches++
				}
			}
			if matches == 0 {
				return fmt.Sprintf("%s matches no %s branch", path, kw)
			}
			if kw == "oneOf" && matches > 1 {
				return fmt.Sprintf("%s matches several oneOf branches", path)
			}
		}
	}

	return ""
}

func extractStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

func jsonTypeOf(v any) string {
	if v == nil {
		return "null"
	}
	switch v.(type) {
	case bool:
		return "boolean"
	case float64, int, int64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func isIntegerValue(v any) bool {
	switch n := v.(type) {
	case int, int64:
		return true
	case float64:
		return !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n
	default:
		return false
	}
}

func intFromAny(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if math.Trunc(n) == n {
			return int(n), true
		}
	}
	return 0, false
}

func floatFromAny(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func jsonEqual(a, b any) bool {
	ba, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ba) == string(bb)
}
