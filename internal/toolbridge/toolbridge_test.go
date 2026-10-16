package toolbridge

import (
	"strings"
	"testing"
)

func sampleTools() []any {
	return []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "read_file",
				"description": "Read a file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":  map[string]any{"type": "string"},
						"lines": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
					},
					"required":             []any{"path"},
					"additionalProperties": false,
				},
			},
		},
	}
}

func TestValidateToolRequestAndInstructions(t *testing.T) {
	instr, err := BuildToolInstructions(sampleTools(), "none")
	if err != nil || instr != "" {
		t.Fatalf("expected empty instructions for tool_choice=none, got %q, err=%v", instr, err)
	}

	_, err = ValidateToolRequest("not-an-array", "auto")
	if err == nil {
		t.Fatal("expected error for non-array tools")
	}

	instrReq, err := BuildToolInstructions(sampleTools(), "required")
	if err != nil || !strings.Contains(instrReq, "You MUST call at least one tool") {
		t.Fatalf("expected required tool rule in instructions, got %q, err=%v", instrReq, err)
	}
}

func TestInterpretReplyVariantsAndSchemaValidation(t *testing.T) {
	tools := sampleTools()

	// 1. Fenced bare call
	fencedBare := "Let me check that file:\n```json\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/a\"}}\n```"
	res1, err := InterpretReply(fencedBare, tools, "auto")
	if err != nil || res1 == nil || len(res1.Calls) != 1 || res1.Calls[0].Name != "read_file" {
		t.Fatalf("unexpected fenced bare result: %+v, err=%v", res1, err)
	}
	if res1.Text != "Let me check that file:" {
		t.Fatalf("expected preamble preserved in InterpretResult.Text, got %q", res1.Text)
	}

	// 2. XML-wrapped bare call
	xmlBare := "Checking now:\n<tool_call>\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/b\"}}\n</tool_call>"
	res2, err := InterpretReply(xmlBare, tools, "auto")
	if err != nil || res2 == nil || len(res2.Calls) != 1 || res2.Calls[0].Arguments["path"] != "/tmp/b" {
		t.Fatalf("unexpected xml bare result: %+v, err=%v", res2, err)
	}

	// 3. Sanitized HTML pipe & unescaped quotes in tool_calls JSON
	sanitized := "```json\n{\"tool_calls\":[{\"name\":\"read_file\",\"arguments\":{\"path\":\"<Foo Bar |>\"}}]}\n```"
	res3, err := InterpretReply(sanitized, tools, "auto")
	if err != nil || res3 == nil || res3.Calls[0].Arguments["path"] != "<Foo | Bar>" {
		t.Fatalf("expected <Foo | Bar> repair, got %+v, err=%v", res3, err)
	}

	// 4. Unknown tool rejected with 502 tool_call_error
	_, err = InterpretReply(`{"tool_calls":[{"name":"erase_all","arguments":{}}]}`, tools, "auto")
	if err == nil || !strings.Contains(err.Error(), `not among the tools`) {
		t.Fatalf("expected unknown tool error, got %v", err)
	}

	// 5. Schema violation (missing required path or out of bounds integer)
	_, err = InterpretReply(`{"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/x","lines":500}}]}`, tools, "auto")
	if err == nil || !strings.Contains(err.Error(), "violate the tool schema") {
		t.Fatalf("expected schema violation error, got %v", err)
	}

	// 6. Required tool missing -> 502 tool_call_error
	_, err = InterpretReply("Plain text answer without calling any tool.", tools, "required")
	if err == nil || !strings.Contains(err.Error(), "did not return the required tool call") {
		t.Fatalf("expected required tool call error, got %v", err)
	}
}

func TestAntiLeakToolCallShapesAndRepairs(t *testing.T) {
	tools := sampleTools()

	// 1. Top-level JSON array of calls: [{"name": "read_file", "arguments": {...}}]
	arrReply := "```json\n[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/arr\"}}]\n```"
	resArr, err := InterpretReply(arrReply, tools, "auto")
	if err != nil || resArr == nil || len(resArr.Calls) != 1 || resArr.Calls[0].Arguments["path"] != "/tmp/arr" {
		t.Fatalf("expected top-level array call to parse, got %+v err=%v", resArr, err)
	}

	// 2. Alternative envelope and argument keys ("tool_call", "tool", "input") + trailing comma + multiline string
	altReply := "I will read it now:\n```json\n{\"tool_call\":{\"tool\":\"read_file\",\"input\":{\"path\":\"/tmp/line1\\nline2\",},}}\n```\nDone."
	resAlt, err := InterpretReply(altReply, tools, "auto")
	if err != nil || resAlt == nil || len(resAlt.Calls) != 1 || resAlt.Calls[0].Name != "read_file" {
		t.Fatalf("expected alt envelope + trailing comma repair to parse, got %+v err=%v", resAlt, err)
	}

	// 3. Bare call surrounded by preamble and postamble without fences (recognized because tool name is in allow)
	bareSurrounded := "Let me check that file first.\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"/tmp/bare\"}}\nI will inspect the output next."
	resBare, err := InterpretReply(bareSurrounded, tools, "auto")
	if err != nil || resBare == nil || len(resBare.Calls) != 1 || resBare.Calls[0].Arguments["path"] != "/tmp/bare" {
		t.Fatalf("expected bare call with postamble to be captured instead of leaking, got %+v err=%v", resBare, err)
	}

	// 4. SafeStreamablePrefix holds back short text and tool calls, but streams long plain prose
	if got := SafeStreamablePrefix(`{"tool_calls":[{"name":"read_file"}]}`); got != "" {
		t.Fatalf("expected empty prefix for JSON tool call, got %q", got)
	}
	longProse := strings.Repeat("This is a clean plain-text final summary sentence with no tool calls. ", 8)
	safe := SafeStreamablePrefix(longProse)
	if safe == "" || len(safe) >= len(longProse) || !strings.HasPrefix(longProse, safe) {
		t.Fatalf("expected non-empty safe prefix shorter than full text, got len=%d (full=%d)", len(safe), len(longProse))
	}
}

func TestProseExamplesSpacedStreamingAndSchemaRefEnforcement(t *testing.T) {
	tools := sampleTools()

	// 1. Inline prose examples must never be executed as tool calls
	for _, example := range []string{
		`Here is an example, do not execute: {"name":"read_file","arguments":{"path":"/tmp/example"}}`,
		`The envelope format is: {"tool_calls":[{"name":"read_file","arguments":{"path":"/tmp/example"}}]} — send it to the harness.`,
	} {
		res, err := InterpretReply(example, tools, "auto")
		if err != nil || res != nil {
			t.Fatalf("expected inline prose example not to trigger tool call, got res=%+v err=%v", res, err)
		}
	}

	// 2. Spaced JSON tool call after preamble must never leak through SafeStreamablePrefix
	spaced := `Preamble text before tool call: { "name": "read_file", "arguments": {"path": "` + strings.Repeat("x", 500) + `"}}`
	if got := SafeStreamablePrefix(spaced); got != "" {
		t.Fatalf("expected SafeStreamablePrefix to hold back spaced tool call JSON, got %q", got)
	}

	// 3. additionalProperties: false without properties and $ref validation
	noPropsSchema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	}
	if msg := CheckSchema(noPropsSchema, map[string]any{"unexpected": true}, "arguments"); msg == "" {
		t.Fatal("expected additionalProperties:false without properties to reject unexpected key")
	}

	refSchema := map[string]any{
		"$ref": "#/$defs/allowed",
		"$defs": map[string]any{
			"allowed": map[string]any{
				"const": "safe",
			},
		},
	}
	if msg := CheckSchema(refSchema, "unsafe", "arguments"); msg == "" {
		t.Fatal("expected $ref schema constraint violation to be rejected")
	}
	if msg := CheckSchema(refSchema, "safe", "arguments"); msg != "" {
		t.Fatalf("expected valid $ref value to pass, got %q", msg)
	}
}

func TestLeadingSpaceKeysAndSchemaInferredToolName(t *testing.T) {
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
					"required": []any{"command", "i"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "write",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
						"i":       map[string]any{"type": "string"},
					},
					"required": []any{"path", "i"},
				},
			},
		},
	}

	// Exact SentencePiece leading-space key reproduction: {" name":"bash", ...}
	raw := `{"tool_calls":[{" name":"bash","arguments":{"command":"npm install","i":"Installing npm dependencies"}},{"name":"write","arguments":{"content":"<!doctype html>\n<html></html>\n","i":"Writing index.html","path":"index.html"}}]}`
	res, err := InterpretReply(raw, tools, "auto")
	if err != nil || res == nil || len(res.Calls) != 2 {
		t.Fatalf("expected 2 parsed tool calls from leading-space key payload, got res=%+v err=%v", res, err)
	}
	if res.Calls[0].Name != "bash" || res.Calls[0].Arguments["command"] != "npm install" {
		t.Fatalf("unexpected first call: %+v", res.Calls[0])
	}
	if res.Calls[1].Name != "write" || res.Calls[1].Arguments["path"] != "index.html" {
		t.Fatalf("unexpected second call: %+v", res.Calls[1])
	}

	// Missing "name" on 2nd call inferred from schema + flat arguments on 1st call
	inferredRaw := `{"tool_calls":[{"name":"bash","command":"npm test","i":"Running tests"},{"path":"src/app.ts","content":"console.log(1);","i":"Writing app.ts"}]}`
	resInf, err := InterpretReply(inferredRaw, tools, "auto")
	if err != nil || resInf == nil || len(resInf.Calls) != 2 {
		t.Fatalf("expected 2 parsed tool calls from flat + schema-inferred payload, got res=%+v err=%v", resInf, err)
	}
	if resInf.Calls[0].Name != "bash" || resInf.Calls[1].Name != "write" {
		t.Fatalf("expected bash and write, got %+v", resInf.Calls)
	}
}

func TestLiveToolCallStreamerIncrementalAndMultiCall(t *testing.T) {
	tools := []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "write_file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
					},
					"required": []any{"path", "content"},
				},
			},
		},
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
	}

	var deltas []map[string]any
	streamer := NewLiveToolCallStreamer(tools, "auto", func(d map[string]any) {
		deltas = append(deltas, d)
	})

	frame1 := "```json\n{\"tool_calls\":[{\"name\":\"write_file\",\"arguments\":{\"path\":\"src/scene.ts\",\"content\":\"import * as THREE from 'three';\\nexport class SceneManager {\\n  private scene: THREE.Scene;\\n  constructor() {\\n    this.scene = new THREE.Scene();"
	frame2 := frame1 + "\\n    console.log('initialized');\\n  }\\n}\"}},{\"name\":\"read_file\",\"arguments\":{\"path\":\"package.json\"}}"
	frame3 := frame2 + "]}\n```"

	streamer.Push(frame1)
	if len(deltas) < 2 {
		t.Fatalf("expected header + live partial arguments on frame1, got %d deltas: %+v", len(deltas), deltas)
	}
	deltasAfterFrame1 := len(deltas)

	streamer.Push(frame2)
	if len(deltas) <= deltasAfterFrame1 {
		t.Fatalf("expected additional deltas on frame2 as call 0 finishes and call 1 completes, got %d", len(deltas))
	}

	res, err := InterpretReply(frame3, tools, "auto")
	if err != nil {
		t.Fatalf("InterpretReply failed: %v", err)
	}
	oa := streamer.Finalize(res.Calls, frame3)
	if len(oa) != 2 {
		t.Fatalf("expected 2 finalized OpenAI tool calls, got %d", len(oa))
	}

	// Reconstruct arguments per tool index from streamed deltas
	reconstructed := map[int]string{}
	names := map[int]string{}
	for _, d := range deltas {
		tcs, _ := d["tool_calls"].([]map[string]any)
		for _, tc := range tcs {
			idx := tc["index"].(int)
			fn := tc["function"].(map[string]any)
			if n, ok := fn["name"].(string); ok && n != "" {
				names[idx] = n
			}
			if a, ok := fn["arguments"].(string); ok {
				reconstructed[idx] += a
			}
		}
	}
	if names[0] != "write_file" || names[1] != "read_file" {
		t.Fatalf("unexpected streamed tool names: %+v", names)
	}
	if reconstructed[0] != oa[0].Function.Arguments {
		t.Fatalf("call 0 streamed args mismatch:\ngot:  %s\nwant: %s", reconstructed[0], oa[0].Function.Arguments)
	}
	if reconstructed[1] != oa[1].Function.Arguments {
		t.Fatalf("call 1 streamed args mismatch:\ngot:  %s\nwant: %s", reconstructed[1], oa[1].Function.Arguments)
	}
}
