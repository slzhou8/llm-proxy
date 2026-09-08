package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"llmproxy/config"
)

func TestRequestOpenAIToAnthropic(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"Hello"},
			{"role":"assistant","content":"Hi","tool_calls":[{"id":"call1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}}]},
			{"role":"tool","tool_call_id":"call1","content":"sunny"}
		],
		"temperature":0.5
	}`)
	out, err := RequestOpenAIToAnthropic(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var a struct {
		Model     string `json:"model"`
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &a); err != nil {
		t.Fatalf("decode anthropic: %v", err)
	}
	if a.System != "You are helpful." {
		t.Errorf("system not extracted: %q", a.System)
	}
	if a.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens default not applied: %d", a.MaxTokens)
	}
	// Expect: user(Hello), assistant(tool_use), user(tool_result)
	if len(a.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %#v", len(a.Messages), a.Messages)
	}
	if a.Messages[0].Role != "user" {
		t.Errorf("msg0 role=%s", a.Messages[0].Role)
	}
	asst, _ := a.Messages[1].Content.([]any)
	if len(asst) != 2 {
		t.Fatalf("expected 2 assistant blocks (text + tool_use), got %d", len(asst))
	}
	tb0 := asst[0].(map[string]any)
	if tb0["type"] != "text" || tb0["text"] != "Hi" {
		t.Errorf("assistant text block wrong: %#v", tb0)
	}
	tb1 := asst[1].(map[string]any)
	if tb1["type"] != "tool_use" || tb1["name"] != "get_weather" {
		t.Errorf("assistant tool_use block wrong: %#v", tb1)
	}
	usr, _ := a.Messages[2].Content.([]any)
	tb := usr[0].(map[string]any)
	if tb["type"] != "tool_result" || tb["tool_use_id"] != "call1" {
		t.Errorf("tool_result block wrong: %#v", tb)
	}
}

func TestRequestOpenAIToAnthropicImage(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`)
	out, err := RequestOpenAIToAnthropic(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !strings.Contains(string(out), `"type":"image"`) || !strings.Contains(string(out), `"base64"`) {
		t.Errorf("image block not converted: %s", out)
	}
}

func TestResponseAnthropicToOpenAI(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant","model":"claude",
		"content":[
			{"type":"text","text":"Hi there"},
			{"type":"tool_use","id":"t1","name":"f","input":{"x":1}}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":10,"output_tokens":5}
	}`)
	out, err := ResponseAnthropicToOpenAI(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var o struct {
		Object string `json:"object"`
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   *string
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &o); err != nil {
		t.Fatalf("decode openai: %v", err)
	}
	if o.Object != "chat.completion" {
		t.Errorf("object=%s", o.Object)
	}
	if o.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason=%s", o.Choices[0].FinishReason)
	}
	if o.Choices[0].Message.Content != nil {
		t.Errorf("content should be null when tool calls present")
	}
	if len(o.Choices[0].Message.ToolCalls) != 1 || o.Choices[0].Message.ToolCalls[0].ID != "t1" {
		t.Errorf("tool call wrong: %#v", o.Choices[0].Message.ToolCalls)
	}
	if o.Usage.PromptTokens != 10 || o.Usage.CompletionTokens != 5 {
		t.Errorf("usage wrong: %#v", o.Usage)
	}
}

func TestTranslateStreamAnthropicToOpenAI(t *testing.T) {
	// A minimal but representative Anthropic SSE stream: text + a tool call + usage.
	src := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_x","model":"claude","usage":{"input_tokens":7}}}`,
		"",
		"event: content_block_start",
		`data: {"index":0,"content_block":{"type":"text"}}`,
		"",
		"event: content_block_delta",
		`data: {"index":0,"delta":{"type":"text_delta","text":"Hello "}}`,
		"",
		"event: content_block_delta",
		`data: {"index":0,"delta":{"type":"text_delta","text":"world"}}`,
		"",
		"event: content_block_start",
		`data: {"index":1,"content_block":{"type":"tool_use","id":"t1","name":"f"}}`,
		"",
		"event: content_block_delta",
		`data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		"",
		"event: content_block_delta",
		`data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		"",
		"event: content_block_stop",
		"",
		"event: message_delta",
		`data: {"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
		"",
		"event: message_stop",
		"",
	}, "\n")

	var dst strings.Builder
	if err := TranslateStreamAnthropicToOpenAI(&dst, strings.NewReader(src)); err != nil {
		t.Fatalf("translate stream: %v", err)
	}
	out := dst.String()
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("missing [DONE]:\n%s", out)
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Errorf("missing role chunk:\n%s", out)
	}
	// Text arrives as separate SSE deltas; the client concatenates them.
	if !strings.Contains(out, "Hello ") || !strings.Contains(out, "world") {
		t.Errorf("missing streamed text deltas:\n%s", out)
	}
	if !strings.Contains(out, `"tool_calls"`) || !strings.Contains(out, `"name":"f"`) {
		t.Errorf("missing tool call chunk:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("missing finish_reason:\n%s", out)
	}
	// The two input_json_delta pieces must each appear; the client joins them
	// into the valid JSON {"a":1}. In the SSE JSON the quotes are escaped, so we
	// match the escaped fragments.
	if !strings.Contains(out, `a\":`) || !strings.Contains(out, "1}") {
		t.Errorf("tool arguments not streamed correctly: %s", out)
	}
}

// TestSnapshotGroupIncludesTranslate verifies an OpenAI-speaking caller can reach
// an Anthropic upstream flagged TranslateToOpenAI, but not a plain Anthropic one.
func TestSnapshotGroupIncludesTranslate(t *testing.T) {
	p := newTestProxy(
		config.Upstream{Name: "plain", Protocol: config.ProtocolOpenAI, Enabled: true, BaseURL: "http://x", APIKey: "k"},
		config.Upstream{Name: "bridge", Protocol: config.ProtocolAnthropic, Enabled: true, BaseURL: "http://y", APIKey: "k", TranslateToOpenAI: true},
		config.Upstream{Name: "anth-no-bridge", Protocol: config.ProtocolAnthropic, Enabled: true, BaseURL: "http://z", APIKey: "k"},
	)
	grp := p.snapshotGroup(config.ProtocolOpenAI)
	names := map[string]bool{}
	for _, u := range grp {
		names[u.Name] = true
	}
	if !names["bridge"] {
		t.Errorf("translate-enabled anthropic upstream must be reachable by OpenAI clients; group=%v", names)
	}
	if names["anth-no-bridge"] {
		t.Errorf("non-bridge anthropic upstream must NOT be in OpenAI group")
	}
}
