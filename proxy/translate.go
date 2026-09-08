package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// This file implements the OpenAI <-> Anthropic protocol bridge used when an
// Upstream has Protocol=anthropic and TranslateToOpenAI=true while the caller
// speaks OpenAI. Both directions are supported for streaming and non-streaming
// calls. All functions are pure transforms: a nil/empty input leaves behaviour
// untouched elsewhere, so an upstream without the flag never reaches this code.

const defaultMaxTokens = 4096

// ---------------------------------------------------------------------------
// Request: OpenAI chat/completions -> Anthropic messages
// ---------------------------------------------------------------------------

type oaiReq struct {
	Model       string          `json:"model"`
	Messages    []oaiMsg        `json:"messages"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Stop        json.RawMessage `json:"stop,omitempty"`
	Tools       []oaiTool       `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
}

type oaiMsg struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []oaiToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type oaiContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type antReq struct {
	Model         string          `json:"model"`
	Messages      []antMsg        `json:"messages"`
	System        string          `json:"system,omitempty"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []antTool       `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
}

type antMsg struct {
	Role    string     `json:"role"`
	Content []antBlock `json:"content"`
}

type antBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *antImageSource `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type antImageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type antTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// RequestOpenAIToAnthropic converts an OpenAI chat/completions request body into
// an Anthropic messages request body.
func RequestOpenAIToAnthropic(body []byte) ([]byte, error) {
	var req oaiReq
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("openai request is not valid JSON: %w", err)
	}

	out := antReq{
		Model:     req.Model,
		MaxTokens: defaultMaxTokens,
		Stream:    req.Stream,
	}
	if req.MaxTokens != nil {
		out.MaxTokens = *req.MaxTokens
	}
	out.Temperature = req.Temperature
	out.TopP = req.TopP

	if len(req.Stop) > 0 {
		var s string
		if err := json.Unmarshal(req.Stop, &s); err == nil {
			out.StopSequences = []string{s}
		} else {
			var ss []string
			if err := json.Unmarshal(req.Stop, &ss); err == nil {
				out.StopSequences = ss
			}
		}
	}

	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			schema := t.Function.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{}`)
			}
			out.Tools = append(out.Tools, antTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: schema,
			})
		}
	}
	if len(req.ToolChoice) > 0 {
		var sc string
		if err := json.Unmarshal(req.ToolChoice, &sc); err == nil {
			switch sc {
			case "none":
				out.ToolChoice = json.RawMessage(`"none"`)
			case "auto":
				out.ToolChoice = json.RawMessage(`"auto"`)
			}
		} else {
			var fc struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(req.ToolChoice, &fc); err == nil && fc.Function.Name != "" {
				out.ToolChoice = json.RawMessage(fmt.Sprintf(`{"type":"tool","name":%q}`, fc.Function.Name))
			}
		}
	}

	var system strings.Builder
	var msgs []antMsg
	var pendingToolResults []antBlock // buffered tool_result blocks for the next user turn

	flushTools := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		msgs = append(msgs, antMsg{Role: "user", Content: pendingToolResults})
		pendingToolResults = nil
	}

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			if txt, ok := rawString(m.Content); ok {
				if system.Len() > 0 {
					system.WriteString("\n\n")
				}
				system.WriteString(txt)
			}
		case "tool":
			content := ""
			if txt, ok := rawString(m.Content); ok {
				content = txt
			}
			pendingToolResults = append(pendingToolResults, antBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   content,
			})
		case "assistant":
			var blocks []antBlock
			if txt, ok := rawString(m.Content); ok && txt != "" {
				blocks = append(blocks, antBlock{Type: "text", Text: txt})
			}
			for _, tc := range m.ToolCalls {
				var input json.RawMessage
				if tc.Function.Arguments != "" {
					input = json.RawMessage(tc.Function.Arguments)
				} else {
					input = json.RawMessage(`{}`)
				}
				blocks = append(blocks, antBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}
			if len(blocks) > 0 {
				msgs = append(msgs, antMsg{Role: "assistant", Content: blocks})
			}
		case "user":
			blocks, err := userContentToAnthropic(m.Content)
			if err != nil {
				return nil, err
			}
			if len(blocks) > 0 {
				flushTools()
				msgs = append(msgs, antMsg{Role: "user", Content: blocks})
			}
		default:
			// Unknown role: best-effort treat as user text.
			if txt, ok := rawString(m.Content); ok {
				flushTools()
				msgs = append(msgs, antMsg{Role: "user", Content: []antBlock{{Type: "text", Text: txt}}})
			}
		}
	}
	flushTools()

	out.System = system.String()
	out.Messages = msgs

	return json.Marshal(out)
}

// userContentToAnthropic turns an OpenAI user message content (string or array
// of text/image parts) into Anthropic content blocks.
func userContentToAnthropic(raw json.RawMessage) ([]antBlock, error) {
	if txt, ok := rawString(raw); ok {
		return []antBlock{{Type: "text", Text: txt}}, nil
	}
	var parts []oaiContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("unsupported user content: %w", err)
	}
	var blocks []antBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, antBlock{Type: "text", Text: p.Text})
		case "image_url":
			src, mediaType, data, url, err := parseImageURL(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			b := antBlock{Type: "image", Source: &antImageSource{Type: src, MediaType: mediaType, Data: data, URL: url}}
			blocks = append(blocks, b)
		}
	}
	return blocks, nil
}

// parseImageURL normalises an OpenAI image_url into an Anthropic image source.
func parseImageURL(u string) (srcType, mediaType, data, url string, err error) {
	if strings.HasPrefix(u, "data:") {
		// data:image/png;base64,xxxx
		comma := strings.Index(u, ",")
		if comma < 0 {
			return "", "", "", "", fmt.Errorf("malformed data URI image")
		}
		meta := u[len("data:"):comma]
		b64 := u[comma+1:]
		mt := meta
		if i := strings.Index(meta, ";"); i >= 0 {
			mt = meta[:i]
		}
		return "base64", mt, b64, "", nil
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return "url", "", "", u, nil
	}
	return "", "", "", "", fmt.Errorf("unsupported image url: %s", u)
}

// rawString reports whether raw is a JSON string and returns it (without quotes).
func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// ---------------------------------------------------------------------------
// Response: Anthropic messages -> OpenAI chat/completion (non-streaming)
// ---------------------------------------------------------------------------

type antResp struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []antRespBlock `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence string         `json:"stop_sequence"`
	Usage        struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type antRespBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type oaiResp struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
}

type oaiChoice struct {
	Index        int       `json:"index"`
	Message      oaiMsgOut `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

type oaiMsgOut struct {
	Role      string           `json:"role"`
	Content   *string          `json:"content"`
	ToolCalls []oaiToolCallOut `json:"tool_calls,omitempty"`
}

type oaiToolCallOut struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ResponseAnthropicToOpenAI converts an Anthropic messages response body into an
// OpenAI chat/completion response body.
func ResponseAnthropicToOpenAI(body []byte) ([]byte, error) {
	var r antResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic response is not valid JSON: %w", err)
	}

	out := oaiResp{
		ID:      r.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   r.Model,
	}
	var content strings.Builder
	var toolCalls []oaiToolCallOut
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			content.WriteString(b.Text)
		case "tool_use":
			args := string(b.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, oaiToolCallOut{
				ID:   b.ID,
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{Name: b.Name, Arguments: args},
			})
		}
	}

	msg := oaiMsgOut{Role: "assistant"}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	} else {
		s := content.String()
		msg.Content = &s
	}
	out.Choices = []oaiChoice{{
		Index:        0,
		Message:      msg,
		FinishReason: mapStopReason(r.StopReason),
	}}
	out.Usage = oaiUsage{
		PromptTokens:     r.Usage.InputTokens,
		CompletionTokens: r.Usage.OutputTokens,
		TotalTokens:      r.Usage.InputTokens + r.Usage.OutputTokens,
	}
	return json.Marshal(out)
}

func mapStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

// ---------------------------------------------------------------------------
// Response: Anthropic SSE -> OpenAI SSE (streaming, incremental)
// ---------------------------------------------------------------------------

type anthropicSSETranslator struct {
	src          io.ReadCloser
	br           *bufio.Reader
	msgID        string
	model        string
	inputTokens  int
	outputTokens int
	toolIdx      int
	blockToolIdx map[int]int // anthropic content_block index -> openai tool index
	out          []byte      // pending translated output bytes
	pos          int
	done         bool
	finished     bool
}

// TranslateStreamAnthropicToOpenAI reads an Anthropic SSE stream from src and
// writes an equivalent OpenAI SSE stream to dst, translating events as they
// arrive so latency is preserved. It is implemented as a synchronous pull
// (io.Copy drives the translator), so there is no second goroutine and no
// producer/consumer deadlock when the consumer is itself a streaming writer.
func TranslateStreamAnthropicToOpenAI(dst io.Writer, src io.Reader) error {
	tr := &anthropicSSETranslator{
		src:          newReadCloser(src),
		br:           bufio.NewReader(src),
		blockToolIdx: map[int]int{},
	}
	_, err := io.Copy(dst, tr)
	return err
}

// Read pulls the next batch of upstream SSE events, translates them into OpenAI
// chunks, and returns ready bytes. Bytes are produced incrementally per event so
// downstream latency is preserved. At upstream EOF a final [DONE] is emitted.
func (t *anthropicSSETranslator) Read(p []byte) (int, error) {
	if t.pos >= len(t.out) {
		if t.finished {
			return 0, io.EOF
		}
		if err := t.fill(); err != nil {
			return 0, err
		}
		if t.pos >= len(t.out) {
			if t.finished {
				return 0, io.EOF
			}
			// Event emitted nothing (e.g. a text content_block_start); keep pulling.
			return 0, nil
		}
	}
	n := copy(p, t.out[t.pos:])
	t.pos += n
	if t.pos >= len(t.out) {
		t.out = t.out[:0]
		t.pos = 0
	}
	return n, nil
}

// Close releases the underlying upstream body.
func (t *anthropicSSETranslator) Close() error {
	if t.src != nil {
		return t.src.Close()
	}
	return nil
}

// fill parses one complete SSE event block from the upstream and appends the
// translated OpenAI chunk(s) to t.out.
func (t *anthropicSSETranslator) fill() error {
	var event, data string
	for {
		line, err := t.br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			if event != "" || data != "" {
				t.handle(event, data)
			}
			if err != nil {
				if err == io.EOF {
					if !t.done {
						t.writeDONE()
					}
					t.finished = true
				}
				// Return nil so Read can still flush any pending out bytes.
				return nil
			}
			return nil
		}
		if strings.HasPrefix(trimmed, "event:") {
			event = strings.TrimSpace(trimmed[len("event:"):])
		} else if strings.HasPrefix(trimmed, "data:") {
			data = strings.TrimSpace(trimmed[len("data:"):])
		}
		// lines starting with ':' are SSE comments; ignore.
		if err != nil {
			if err == io.EOF {
				if event != "" || data != "" {
					t.handle(event, data)
				}
				if !t.done {
					t.writeDONE()
				}
				t.finished = true
				return nil
			}
			return err
		}
	}
}

type nopReadCloser struct{ io.Reader }

func (nopReadCloser) Close() error { return nil }

func newReadCloser(r io.Reader) io.ReadCloser {
	if rc, ok := r.(io.ReadCloser); ok {
		return rc
	}
	return nopReadCloser{Reader: r}
}

func (t *anthropicSSETranslator) writeChunk(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	t.out = append(t.out, []byte("data: ")...)
	t.out = append(t.out, b...)
	t.out = append(t.out, []byte("\n\n")...)
}

func (t *anthropicSSETranslator) writeDONE() {
	t.out = append(t.out, []byte("data: [DONE]\n\n")...)
	t.done = true
}

type oaiChunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []oaiChunkChoice `json:"choices"`
	Usage   *oaiUsage        `json:"usage,omitempty"`
}

type oaiChunkChoice struct {
	Index        int           `json:"index"`
	Delta        oaiChunkDelta `json:"delta"`
	FinishReason *string       `json:"finish_reason,omitempty"`
}

type oaiChunkDelta struct {
	Role      string             `json:"role,omitempty"`
	Content   string             `json:"content,omitempty"`
	ToolCalls []oaiChunkToolCall `json:"tool_calls,omitempty"`
}

type oaiChunkToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

func (t *anthropicSSETranslator) handle(event, data string) {
	switch event {
	case "message_start":
		var m struct {
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		_ = json.Unmarshal([]byte(data), &m)
		t.msgID = m.Message.ID
		t.model = m.Message.Model
		t.inputTokens = m.Message.Usage.InputTokens
		t.writeChunk(oaiChunk{
			ID:      t.msgID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   t.model,
			Choices: []oaiChunkChoice{{
				Index: 0,
				Delta: oaiChunkDelta{Role: "assistant", Content: ""},
			}},
		})
	case "content_block_start":
		var m struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		_ = json.Unmarshal([]byte(data), &m)
		if m.ContentBlock.Type == "tool_use" {
			idx := t.toolIdx
			t.toolIdx++
			t.blockToolIdx[m.Index] = idx
			tc := oaiChunkToolCall{Index: idx, ID: m.ContentBlock.ID, Type: "function"}
			tc.Function.Name = m.ContentBlock.Name
			t.writeChunk(oaiChunk{
				ID:      t.msgID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   t.model,
				Choices: []oaiChunkChoice{{
					Index: 0,
					Delta: oaiChunkDelta{ToolCalls: []oaiChunkToolCall{tc}},
				}},
			})
		}
	case "content_block_delta":
		var m struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(data), &m)
		switch m.Delta.Type {
		case "text_delta":
			t.writeChunk(oaiChunk{
				ID:      t.msgID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   t.model,
				Choices: []oaiChunkChoice{{
					Index: 0,
					Delta: oaiChunkDelta{Content: m.Delta.Text},
				}},
			})
		case "input_json_delta":
			idx, ok := t.blockToolIdx[m.Index]
			if !ok {
				idx = 0
			}
			tc := oaiChunkToolCall{Index: idx}
			tc.Function.Arguments = m.Delta.PartialJSON
			t.writeChunk(oaiChunk{
				ID:      t.msgID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   t.model,
				Choices: []oaiChunkChoice{{
					Index: 0,
					Delta: oaiChunkDelta{ToolCalls: []oaiChunkToolCall{tc}},
				}},
			})
		}
	case "message_delta":
		var m struct {
			Delta struct {
				StopReason   string `json:"stop_reason"`
				StopSequence string `json:"stop_sequence"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal([]byte(data), &m)
		t.outputTokens = m.Usage.OutputTokens
		fr := mapStopReason(m.Delta.StopReason)
		t.writeChunk(oaiChunk{
			ID:      t.msgID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   t.model,
			Choices: []oaiChunkChoice{{
				Index:        0,
				Delta:        oaiChunkDelta{},
				FinishReason: &fr,
			}},
			Usage: &oaiUsage{
				PromptTokens:     t.inputTokens,
				CompletionTokens: t.outputTokens,
				TotalTokens:      t.inputTokens + t.outputTokens,
			},
		})
	case "message_stop":
		t.writeDONE()
	}
}
