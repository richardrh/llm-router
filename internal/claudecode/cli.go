package claudecode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// cliRun is the outcome of one CLI invocation.
type cliRun struct {
	Text    string
	Usage   Usage
	CostUSD float64
	HasCost bool
}

// buildCLICommand substitutes the prompt for the {prompt} placeholder.
// It returns an error if {prompt} is absent or appears more than once.
func buildCLICommand(argv []string, prompt string) ([]string, error) {
	found := -1
	count := 0
	for i, arg := range argv {
		if strings.Contains(arg, "{prompt}") {
			count += strings.Count(arg, "{prompt}")
			found = i
		}
	}
	if count == 0 {
		return nil, errors.New("CLI command is missing the {prompt} placeholder")
	}
	if count > 1 {
		return nil, errors.New("CLI command must contain exactly one {prompt} placeholder")
	}
	out := append([]string(nil), argv...)
	out[found] = strings.Replace(out[found], "{prompt}", prompt, 1)
	return out, nil
}

// newCLIPrompt renders an OpenAI-shaped request into a single prompt for an
// agent CLI. System and developer instructions are placed first, followed by
// labelled user and assistant turns.
func newCLIPrompt(fields map[string]json.RawMessage) (string, error) {
	raw, ok := fields["messages"]
	if !ok {
		return "", errors.New("request has no messages")
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &messages); err != nil {
		return "", fmt.Errorf("invalid messages: %w", err)
	}

	var leads []string
	var turns []string
	for _, message := range messages {
		text := cliMessageText(message.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		switch message.Role {
		case "system", "developer":
			leads = append(leads, text)
		case "user":
			turns = append(turns, "User: "+text)
		case "assistant":
			turns = append(turns, "Assistant: "+text)
		}
	}
	parts := append(leads, turns...)
	if len(parts) == 0 {
		return "", errors.New("request contains no text")
	}
	return strings.Join(parts, "\n\n"), nil
}

// newCLILastPrompt extracts only the newest user turn for a persistent Claude
// Code process. Claude Code already owns the preceding transcript.
func newCLILastPrompt(fields map[string]json.RawMessage) (string, error) {
	raw, ok := fields["messages"]
	if !ok {
		return "", errors.New("request has no messages")
	}
	var messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &messages); err != nil {
		return "", fmt.Errorf("invalid messages: %w", err)
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		text := cliMessageText(messages[i].Content)
		if strings.TrimSpace(text) != "" {
			return text, nil
		}
	}
	return "", errors.New("request contains no user text")
}

func cliMessageText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			out.WriteString(part.Text)
		}
	}
	return out.String()
}

const cliStderrLimit = 4 << 10

type cliStderr struct {
	buf bytes.Buffer
}

func (s *cliStderr) Write(p []byte) (int, error) {
	n := len(p)
	remaining := cliStderrLimit - s.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = s.buf.Write(p)
	}
	return n, nil
}

func (s *cliStderr) String() string { return s.buf.String() }

func killCLIProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// The command is its own process group, so shell children cannot keep the
	// stdout pipe open after cancellation.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

// runCLICommand runs argv, decoding the stream-json NDJSON contract from
// stdout, calling onText for each text delta as it arrives. It returns the
// final result.
func runCLICommand(ctx context.Context, argv []string, onText func(string) error) (cliRun, error) {
	if len(argv) == 0 || argv[0] == "" {
		return cliRun{}, errors.New("CLI command is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return cliRun{}, fmt.Errorf("capture CLI stdout: %w", err)
	}
	var stderr cliStderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return cliRun{}, fmt.Errorf("start CLI command: %w", err)
	}

	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killCLIProcess(cmd)
		case <-cancelDone:
		}
	}()

	var result cliRun
	var callbackErr error
	var resultErr error
	seenResult := false
	reader := bufio.NewReader(stdout)
	var scanErr error
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var event struct {
				Type  string `json:"type"`
				Event struct {
					Type  string `json:"type"`
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				} `json:"event"`
				IsError      bool     `json:"is_error"`
				Result       string   `json:"result"`
				TotalCostUSD *float64 `json:"total_cost_usd"`
			}
			if json.Unmarshal(line, &event) == nil {
				switch event.Type {
				case "stream_event":
					if event.Event.Type == "content_block_delta" && event.Event.Delta.Type == "text_delta" && onText != nil {
						if err := onText(event.Event.Delta.Text); err != nil {
							callbackErr = err
							killCLIProcess(cmd)
						}
					}
				case "result":
					seenResult = true
					result.Text = event.Result
					if event.TotalCostUSD != nil {
						result.CostUSD = *event.TotalCostUSD
						result.HasCost = true
					}
					result.Usage = Usage{}
					result.Usage.MergeFromJSON(line)
					if event.IsError {
						resultErr = errors.New("CLI returned an error result")
					}
				}
			}
		}
		if callbackErr != nil {
			break
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				scanErr = readErr
			}
			break
		}
	}
	close(cancelDone)
	waitErr := cmd.Wait()

	if err := ctx.Err(); err != nil {
		return cliRun{}, err
	}
	if callbackErr != nil {
		return cliRun{}, callbackErr
	}
	if scanErr != nil {
		return cliRun{}, cliCommandError("read CLI output", scanErr, stderr.String())
	}
	if waitErr != nil {
		return cliRun{}, cliCommandError("CLI command failed", waitErr, stderr.String())
	}
	if resultErr != nil {
		return cliRun{}, cliCommandError("CLI returned an error result", resultErr, stderr.String())
	}
	if !seenResult {
		return cliRun{}, cliCommandError("CLI output had no result", errors.New("missing result line"), stderr.String())
	}
	return result, nil
}

func cliCommandError(prefix string, err error, stderr string) error {
	if stderr == "" {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return fmt.Errorf("%s: %w (stderr: %s)", prefix, err, stderr)
}

type cliOpenAIStream struct {
	alias   string
	id      string
	created int64
}

func newCLIOpenAIStream(alias string) *cliOpenAIStream {
	return &cliOpenAIStream{alias: alias, id: fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), created: time.Now().Unix()}
}

func (s *cliOpenAIStream) chunk(delta map[string]interface{}, finish interface{}, usage interface{}) []byte {
	body := map[string]interface{}{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.alias,
	}
	if delta == nil {
		body["choices"] = []interface{}{}
	} else {
		body["choices"] = []interface{}{map[string]interface{}{"index": 0, "delta": delta, "finish_reason": finish}}
	}
	if usage != nil {
		body["usage"] = usage
	}
	encoded, _ := json.Marshal(body)
	return append([]byte("data: "), append(encoded, '\n', '\n')...)
}

func (s *cliOpenAIStream) start() []byte {
	return s.chunk(map[string]interface{}{"role": "assistant"}, nil, nil)
}

func (s *cliOpenAIStream) text(delta string) []byte {
	return s.chunk(map[string]interface{}{"content": delta}, nil, nil)
}

func cliOpenAIUsage(u Usage) map[string]interface{} {
	prompt := u.TotalInput()
	usage := map[string]interface{}{"prompt_tokens": prompt, "completion_tokens": u.OutputTokens, "total_tokens": prompt + u.OutputTokens}
	details := map[string]interface{}{}
	if u.CacheReadTokens > 0 {
		details["cached_tokens"] = u.CacheReadTokens
	}
	if u.CacheWriteTokens > 0 {
		details["cache_creation_tokens"] = u.CacheWriteTokens
	}
	if len(details) > 0 {
		usage["prompt_tokens_details"] = details
	}
	return usage
}

func (s *cliOpenAIStream) done(r cliRun) []byte {
	out := s.chunk(map[string]interface{}{}, "stop", nil)
	if r.Usage.Total() > 0 {
		out = append(out, s.chunk(nil, nil, cliOpenAIUsage(r.Usage))...)
	}
	return append(out, []byte("data: [DONE]\n\n")...)
}

// cliOpenAIResponse builds a complete OpenAI chat.completion from a finished run.
func cliOpenAIResponse(r cliRun, alias string) []byte {
	body := map[string]interface{}{
		"id": fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()), "object": "chat.completion", "created": time.Now().Unix(), "model": alias,
		"choices": []interface{}{map[string]interface{}{"index": 0, "message": map[string]interface{}{"role": "assistant", "content": r.Text}, "finish_reason": "stop"}},
		"usage":   cliOpenAIUsage(r.Usage),
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

// cliAnthropicResponse builds a complete Anthropic message from a finished run.
func cliAnthropicResponse(r cliRun, alias string) []byte {
	usage := map[string]interface{}{"input_tokens": r.Usage.InputTokens, "output_tokens": r.Usage.OutputTokens}
	if r.Usage.CacheReadTokens > 0 {
		usage["cache_read_input_tokens"] = r.Usage.CacheReadTokens
	}
	if r.Usage.CacheWriteTokens > 0 {
		usage["cache_creation_input_tokens"] = r.Usage.CacheWriteTokens
	}
	body := map[string]interface{}{
		"id": fmt.Sprintf("msg-%d", time.Now().UnixNano()), "type": "message", "role": "assistant", "model": alias,
		"content": []interface{}{map[string]interface{}{"type": "text", "text": r.Text}}, "stop_reason": "end_turn", "stop_sequence": nil, "usage": usage,
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

func (s *cliOpenAIStream) Start() []byte            { return s.start() }
func (s *cliOpenAIStream) Text(delta string) []byte { return s.text(delta) }
func (s *cliOpenAIStream) Done(r cliRun) []byte     { return s.done(r) }
