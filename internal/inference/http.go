package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"llm-router/internal/config"
)

var ErrResponseTooLarge = errors.New("response size limit exceeded")
var ErrIncompleteResponse = errors.New("provider stream ended before completion")

// HTTPRunner adapts the existing public inference implementation, without a
// network hop or a second dispatcher. Protocol framing terminates at this seam.
func HTTPRunner(handler func(http.ResponseWriter, *http.Request, config.APIProtocol)) Run {
	return func(ctx context.Context, request Request, emit Emit) (Result, error) {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if request.Protocol != config.APIOpenAICompletions && request.Protocol != config.APIAnthropicMessages {
			return Result{}, fmt.Errorf("unsupported playground protocol %q", request.Protocol)
		}
		body := struct {
			Model         string          `json:"model"`
			Messages      []Message       `json:"messages"`
			Stream        bool            `json:"stream"`
			MaxTokens     int             `json:"max_tokens,omitempty"`
			StreamOptions map[string]bool `json:"stream_options,omitempty"`
		}{Model: request.Model, Messages: request.Messages, Stream: request.Stream}
		if request.Protocol == config.APIAnthropicMessages {
			body.MaxTokens = request.MaxOutputTokens
			if body.MaxTokens <= 0 {
				body.MaxTokens = 4096
			}
		} else if request.Stream {
			body.StreamOptions = map[string]bool{"include_usage": true}
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return Result{}, err
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://internal/inference", bytes.NewReader(encoded))
		if err != nil {
			return Result{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-OMP-Session", request.ConversationID)
		capture := &responseCapture{ctx: ctx, cancel: cancel, header: make(http.Header), stream: request.Stream, emit: emit, tools: make(map[int]*toolAssembly)}
		handler(capture, req, request.Protocol)
		return capture.finish()
	}
}

type ProviderError struct {
	Status  int
	Message string
}

func (e *ProviderError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("Provider HTTP %d: %s", e.Status, e.Message)
	}
	return "Provider error: " + e.Message
}

type toolAssembly struct {
	id        string
	name      strings.Builder
	arguments strings.Builder
	input     json.RawMessage
}

type responseCapture struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	header    http.Header
	status    int
	stream    bool
	emit      Emit
	err       error
	pending   []byte
	eventData []byte
	total     int
	terminal  bool
	text      strings.Builder
	tools     map[int]*toolAssembly
	result    Result
}

func (c *responseCapture) Header() http.Header { return c.header }
func (c *responseCapture) WriteHeader(status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status == 0 {
		c.status = status
	}
}
func (c *responseCapture) Flush() {}

func (c *responseCapture) fail(err error) error {
	if c.err == nil {
		c.err = err
		c.cancel()
	}
	return c.err
}
func (c *responseCapture) send(event Event) error {
	if c.emit != nil {
		if err := c.emit(event); err != nil {
			return c.fail(err)
		}
	}
	return nil
}
func (c *responseCapture) route() error {
	route := Route{Upstream: c.header.Get("X-Router-Upstream"), Model: c.header.Get("X-Router-Upstream-Model")}
	if route != c.result.Route {
		c.result.Route = route
		return c.send(Event{Kind: RouteSelected, Route: route})
	}
	return nil
}
func (c *responseCapture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return 0, c.err
	}
	if err := c.ctx.Err(); err != nil {
		return 0, c.fail(err)
	}
	if c.total+len(data) > MaxResponseBytes {
		return 0, c.fail(ErrResponseTooLarge)
	}
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if err := c.route(); err != nil {
		return 0, err
	}
	c.total += len(data)
	c.pending = append(c.pending, data...)
	if c.stream && c.status < 400 {
		consumed := 0
		for {
			remaining := c.pending[consumed:]
			index := bytes.IndexByte(remaining, '\n')
			if index < 0 {
				break
			}
			line := bytes.TrimSuffix(remaining[:index], []byte{'\r'})
			if err := c.line(line); err != nil {
				return 0, c.fail(err)
			}
			consumed += index + 1
		}
		if consumed > 0 {
			c.pending = append(c.pending[:0], c.pending[consumed:]...)
		}
	}
	return len(data), nil
}
func (c *responseCapture) line(line []byte) error {
	if len(line) == 0 {
		data := bytes.TrimSpace(c.eventData)
		if bytes.Equal(data, []byte("[DONE]")) {
			c.terminal = true
		} else if len(data) > 0 {
			if err := c.parse(data); err != nil {
				return err
			}
		}
		c.eventData = c.eventData[:0]
		return nil
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil
	}
	data := bytes.TrimPrefix(line, []byte("data:"))
	data = bytes.TrimPrefix(data, []byte(" "))
	if len(c.eventData) > 0 {
		c.eventData = append(c.eventData, '\n')
	}
	c.eventData = append(c.eventData, data...)
	return nil
}
func (c *responseCapture) appendText(text string) error {
	if text == "" {
		return nil
	}
	c.text.WriteString(text)
	return c.send(Event{Kind: TextDelta, Text: text})
}
func (c *responseCapture) tool(index int) (*toolAssembly, error) {
	if index < 0 {
		return nil, errors.New("provider returned a negative tool-call index")
	}
	if existing := c.tools[index]; existing != nil {
		return existing, nil
	}
	tool := &toolAssembly{}
	c.tools[index] = tool
	return tool, nil
}

type openAITool struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type textMessage struct {
	Content      string       `json:"content"`
	ToolCalls    []openAITool `json:"tool_calls"`
	FunctionCall *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function_call"`
}
type contentBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"`
}

func (c *responseCapture) openAITools(message textMessage, complete bool) error {
	for position, part := range message.ToolCalls {
		index := part.Index
		if complete {
			index = position
		}
		tool, err := c.tool(index)
		if err != nil {
			return err
		}
		if part.ID != "" {
			if tool.id != "" && tool.id != part.ID {
				return errors.New("provider changed a tool-call identity")
			}
			tool.id = part.ID
		}
		tool.name.WriteString(part.Function.Name)
		tool.arguments.WriteString(part.Function.Arguments)
	}
	if part := message.FunctionCall; part != nil {
		tool, err := c.tool(0)
		if err != nil {
			return err
		}
		tool.name.WriteString(part.Name)
		tool.arguments.WriteString(part.Arguments)
	}
	return nil
}
func (c *responseCapture) anthropicBlock(index int, block contentBlock) error {
	switch block.Type {
	case "text":
		return c.appendText(block.Text)
	case "tool_use":
		tool, err := c.tool(index)
		if err != nil {
			return err
		}
		tool.id = block.ID
		tool.name.WriteString(block.Name)
		tool.input = block.Input
	}
	return nil
}
func (c *responseCapture) mergeUsage(raw json.RawMessage) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return
	}
	known := false
	for _, key := range []string{"prompt_tokens", "completion_tokens", "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if value, ok := fields[key]; ok && string(value) != "null" {
			known = true
			break
		}
	}
	if !known {
		return
	}
	payload := append(append([]byte(`{"usage":`), raw...), '}')
	if c.result.Usage.MergeFromJSON(payload) {
		c.result.HasUsage = true
	}
}
func (c *responseCapture) parse(data []byte) error {
	var value struct {
		Type         string          `json:"type"`
		Index        int             `json:"index"`
		Error        json.RawMessage `json:"error"`
		Usage        json.RawMessage `json:"usage"`
		Content      []contentBlock  `json:"content"`
		ContentBlock contentBlock    `json:"content_block"`
		Delta        struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
		Message struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Choices []struct {
			Index        int         `json:"index"`
			Delta        textMessage `json:"delta"`
			Message      textMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("invalid provider response: %w", err)
	}
	if len(value.Error) > 0 && string(value.Error) != "null" {
		return &ProviderError{Message: errorMessage(value.Error)}
	}
	if value.Type == "message_stop" {
		c.terminal = true
	}
	c.mergeUsage(value.Usage)
	c.mergeUsage(value.Message.Usage)
	if err := c.appendText(value.Delta.Text); err != nil {
		return err
	}
	if value.Type == "content_block_start" {
		if err := c.anthropicBlock(value.Index, value.ContentBlock); err != nil {
			return err
		}
	}
	if value.Delta.Type == "input_json_delta" {
		tool := c.tools[value.Index]
		if tool == nil {
			return errors.New("provider returned tool arguments without a tool call")
		}
		tool.arguments.WriteString(value.Delta.PartialJSON)
	}
	for index, block := range value.Content {
		if err := c.anthropicBlock(index, block); err != nil {
			return err
		}
	}
	for _, choice := range value.Choices {
		if choice.Index != 0 {
			continue
		} // The playground requests one answer.
		if err := c.appendText(choice.Delta.Content); err != nil {
			return err
		}
		if err := c.appendText(choice.Message.Content); err != nil {
			return err
		}
		if err := c.openAITools(choice.Delta, false); err != nil {
			return err
		}
		if err := c.openAITools(choice.Message, true); err != nil {
			return err
		}
		if choice.FinishReason != "" {
			c.terminal = true
		}
	}
	return nil
}
func errorMessage(raw json.RawMessage) string {
	var object struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &object) == nil && object.Message != "" {
		return object.Message
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}
func (c *responseCapture) finish() (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := func(err error) (Result, error) { c.result.Text = c.text.String(); return c.result, err }
	if c.err != nil {
		return result(c.err)
	}
	if err := c.ctx.Err(); err != nil {
		return result(err)
	}
	if err := c.route(); err != nil {
		return result(err)
	}
	if c.status >= 400 {
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		message := string(c.pending)
		if json.Unmarshal(c.pending, &envelope) == nil && len(envelope.Error) > 0 {
			message = errorMessage(envelope.Error)
		}
		return result(&ProviderError{Status: c.status, Message: message})
	}
	if !c.stream {
		if err := c.parse(c.pending); err != nil {
			return result(err)
		}
	} else {
		if len(c.pending) > 0 {
			if err := c.line(bytes.TrimSuffix(c.pending, []byte{'\r'})); err != nil {
				return result(err)
			}
		}
		if err := c.line(nil); err != nil {
			return result(err)
		}
		if !c.terminal {
			return result(ErrIncompleteResponse)
		}
	}
	indexes := make([]int, 0, len(c.tools))
	for index := range c.tools {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	for _, index := range indexes {
		tool := c.tools[index]
		args := tool.input
		if tool.arguments.Len() > 0 {
			args = json.RawMessage(tool.arguments.String())
		}
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		if !json.Valid(args) {
			return result(errors.New("provider returned incomplete tool arguments"))
		}
		call := ToolCall{ID: tool.id, Name: tool.name.String(), Arguments: args}
		c.result.ToolCalls = append(c.result.ToolCalls, call)
		if err := c.send(Event{Kind: ToolRequested, ToolCall: call}); err != nil {
			return result(err)
		}
	}
	if c.result.HasUsage {
		if err := c.send(Event{Kind: UsageReported, Usage: c.result.Usage}); err != nil {
			return result(err)
		}
	}
	return result(nil)
}
