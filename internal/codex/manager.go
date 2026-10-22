package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrUnsupportedRequest identifies client input that the Codex transport cannot honor.
var ErrUnsupportedRequest = errors.New("unsupported Codex request")

type Manager struct {
	command   []string
	cwd       string
	max       int
	idle      time.Duration
	mu        sync.Mutex
	sessions  map[string]*session
	lost      map[string]bool
	lostOrder []string
	adminMu   sync.Mutex
	admin     *process
	// Authentication failures persist until a new auth action; transport failures
	// are cleared by a successful refresh. Protected by adminMu.
	loginError string
	snapshotMu sync.RWMutex
	snapshot   Account
	closed     bool
}
type session struct {
	p                                          *process
	gate                                       chan struct{}
	users                                      int
	used                                       time.Time
	model, tools, thread, instructions, effort string
	seen                                       int
	pending                                    *message
	callID                                     string
}

func New(command []string, cwd string, maxSessions int, idle time.Duration) *Manager {
	if maxSessions < 1 {
		maxSessions = 16
	}
	return &Manager{command: append([]string{}, command...), cwd: cwd, max: maxSessions, idle: idle, sessions: map[string]*session{}, lost: map[string]bool{}}
}
func (m *Manager) Close() {
	m.adminMu.Lock()
	defer m.adminMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.admin != nil {
		m.admin.close()
	}
	for _, s := range m.sessions {
		s.p.close()
	}
	m.sessions = map[string]*session{}
}
func (m *Manager) Counts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions), m.max
}
func (m *Manager) rememberLost(key string, s *session) {
	if s.thread == "" && s.seen == 0 {
		return
	}
	if !m.lost[key] {
		m.lost[key] = true
		m.lostOrder = append(m.lostOrder, key)
	}
	for len(m.lostOrder) > max(256, m.max*4) {
		delete(m.lost, m.lostOrder[0])
		m.lostOrder = m.lostOrder[1:]
	}
}
func (m *Manager) retire(key string, s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[key] == s {
		delete(m.sessions, key)
		m.rememberLost(key, s)
	}
	s.p.close()
}
func (m *Manager) acquire(ctx context.Context, key string) (*session, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("codex manager closed")
	}
	now := time.Now()
	for k, s := range m.sessions {
		if m.idle > 0 && s.users == 0 && now.Sub(s.used) > m.idle {
			s.p.close()
			delete(m.sessions, k)
			m.rememberLost(k, s)
		}
	}
	s := m.sessions[key]
	if s == nil {
		if m.lost[key] {
			m.mu.Unlock()
			return nil, errors.New("codex conversation expired or was lost; start a new session")
		}
		if len(m.sessions) >= m.max {
			var oldest *session
			var oldkey string
			for k, v := range m.sessions {
				if v.users == 0 && (oldest == nil || v.used.Before(oldest.used)) {
					oldest = v
					oldkey = k
				}
			}
			if oldest == nil {
				m.mu.Unlock()
				return nil, errors.New("codex session capacity reached")
			}
			oldest.p.close()
			delete(m.sessions, oldkey)
			m.rememberLost(oldkey, oldest)
		}
		p, e := startProcess(ctx, m.command, m.cwd)
		if e != nil {
			m.mu.Unlock()
			return nil, e
		}
		s = &session{p: p, gate: make(chan struct{}, 1), used: now}
		m.sessions[key] = s
	}
	s.users++
	m.mu.Unlock()
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		m.mu.Lock()
		s.users--
		m.mu.Unlock()
		return nil, ctx.Err()
	}
	m.mu.Lock()
	if m.sessions[key] != s {
		s.users--
		m.mu.Unlock()
		<-s.gate
		return nil, errors.New("codex session retired while queued")
	}
	m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		m.mu.Lock()
		s.users--
		m.mu.Unlock()
		<-s.gate
		return nil, err
	}
	return s, nil
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
}

func contentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Type string
		Text string
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", fmt.Errorf("%w: message content must be text", ErrUnsupportedRequest)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("%w: unsupported content type %q", ErrUnsupportedRequest, p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}
func dynamicTools(raw json.RawMessage) ([]any, error) {
	var tools []struct {
		Type     string
		Function struct {
			Name, Description string
			Parameters        json.RawMessage
		}
	}
	if len(raw) == 0 {
		return []any{}, nil
	}
	if e := json.Unmarshal(raw, &tools); e != nil {
		return nil, fmt.Errorf("%w: invalid tools: %v", ErrUnsupportedRequest, e)
	}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "function" || t.Function.Name == "" {
			return nil, fmt.Errorf("%w: requires named function tools", ErrUnsupportedRequest)
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, map[string]any{"type": "function", "name": t.Function.Name, "description": t.Function.Description, "inputSchema": schema})
	}
	return out, nil
}
func (m *Manager) Run(ctx context.Context, key, model string, fields map[string]json.RawMessage, onText func(string) error) (result Result, err error) {
	if key == "" {
		return result, errors.New("codex requires a conversation session key")
	}
	if model == "default" {
		models, e := m.Models(ctx)
		if e != nil {
			return result, e
		}
		model = ""
		for _, v := range models {
			if v.Default {
				model = v.ID
				break
			}
		}
		if model == "" {
			return result, errors.New("codex did not report a default model")
		}
	}
	tools, e := dynamicTools(fields["tools"])
	if e != nil {
		return result, e
	}
	toolBytes, _ := json.Marshal(tools)
	var messages []chatMessage
	if e = json.Unmarshal(fields["messages"], &messages); e != nil {
		return result, fmt.Errorf("%w: invalid messages: %v", ErrUnsupportedRequest, e)
	}
	if len(messages) == 0 {
		return result, fmt.Errorf("%w: requires messages", ErrUnsupportedRequest)
	}
	var effort string
	if raw, ok := fields["reasoning_effort"]; ok {
		if json.Unmarshal(raw, &effort) != nil || effort == "" {
			return result, fmt.Errorf("%w: reasoning_effort must be a non-empty model-supported string", ErrUnsupportedRequest)
		}
	}
	if raw, ok := fields["tool_choice"]; ok {
		var choice string
		if json.Unmarshal(raw, &choice) != nil || choice != "auto" {
			return result, fmt.Errorf("%w: supports only tool_choice auto; forced, required, and none are unsupported", ErrUnsupportedRequest)
		}
	}
	for field := range fields {
		switch field {
		case "model", "messages", "tools", "reasoning_effort", "tool_choice", "stream", "stream_options":
		default:
			return result, fmt.Errorf("%w: explicit %s is not supported", ErrUnsupportedRequest, field)
		}
	}
	var instructions strings.Builder
	for _, v := range messages {
		if v.Role == "system" || v.Role == "developer" {
			text, e := contentText(v.Content)
			if e != nil {
				return result, e
			}
			instructions.WriteString(text)
			instructions.WriteString("\n")
		}
	}
	s, e := m.acquire(ctx, key)
	if e != nil {
		return result, e
	}
	stopWatch := s.p.watch(ctx)
	defer func() {
		stopWatch()
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			m.retire(key, s)
		}
		m.mu.Lock()
		s.users--
		s.used = time.Now()
		m.mu.Unlock()
		<-s.gate
	}()
	if s.thread != "" && (s.model != model || s.tools != string(toolBytes) || s.instructions != instructions.String()) {
		return result, fmt.Errorf("%w: model, tools, or instructions changed; start a new session", ErrUnsupportedRequest)
	}
	if s.thread == "" {
		var account struct{ Account *struct{ Type string } }
		if e = s.p.call(ctx, "account/read", map[string]any{"refreshToken": true}, &account); e != nil {
			return result, e
		}
		if account.Account == nil || account.Account.Type != "chatgpt" {
			return result, errors.New("codex requires ChatGPT subscription login; use console Codex sign in")
		}
		var r struct{ Thread struct{ ID string } }
		config := map[string]any{"features.shell_tool": false, "tools.view_image": false, "web_search": "disabled", "mcp_servers": map[string]any{}, "apps": map[string]any{"_default": map[string]any{"enabled": false}}, "agents": map[string]any{"enabled": false}}
		params := map[string]any{"model": model, "modelProvider": "openai", "sandbox": "read-only", "approvalPolicy": "on-request", "approvalsReviewer": "user", "environments": []any{}, "dynamicTools": tools, "config": config, "ephemeral": true, "developerInstructions": instructions.String()}
		if m.cwd != "" {
			params["cwd"] = m.cwd
		}
		if e = s.p.call(ctx, "thread/start", params, &r); e != nil {
			return result, e
		}
		if r.Thread.ID == "" {
			return result, errors.New("codex omitted thread id")
		}
		s.thread = r.Thread.ID
		s.model = model
		s.tools = string(toolBytes)
		s.instructions = instructions.String()
	}
	if len(messages) < s.seen {
		return result, fmt.Errorf("%w: conversation history shortened; start a new session", ErrUnsupportedRequest)
	}
	fresh := messages[s.seen:]
	if len(fresh) == 0 {
		return result, fmt.Errorf("%w: requires new messages", ErrUnsupportedRequest)
	}
	if s.pending != nil && effort != "" && effort != s.effort {
		return result, fmt.Errorf("%w: reasoning_effort cannot change during a pending tool turn", ErrUnsupportedRequest)
	}
	if s.pending == nil {
		s.effort = effort
	}
	if s.pending != nil {
		if len(fresh) != 1 || fresh[0].Role != "tool" || fresh[0].ToolCallID != s.callID {
			return result, fmt.Errorf("%w: requires exactly the matching pending tool result", ErrUnsupportedRequest)
		}
		text, e := contentText(fresh[0].Content)
		if e != nil {
			return result, e
		}
		if e = s.p.send(map[string]any{"id": s.pending.ID, "result": map[string]any{"contentItems": []any{map[string]any{"type": "inputText", "text": text}}, "success": true}}); e != nil {
			return result, e
		}
		s.pending = nil
		s.callID = ""
		s.seen = len(messages)
	} else {
		var b strings.Builder
		for _, v := range fresh {
			if v.Role == "system" || v.Role == "developer" {
				continue
			}
			if v.Role == "tool" {
				return result, fmt.Errorf("%w: received tool result without a pending call", ErrUnsupportedRequest)
			}
			text, e := contentText(v.Content)
			if e != nil {
				return result, e
			}
			if text == "" {
				continue
			}
			if v.Role != "user" && v.Role != "assistant" {
				return result, fmt.Errorf("%w: unsupported role %q", ErrUnsupportedRequest, v.Role)
			}
			b.WriteString(v.Role)
			b.WriteString(":\n")
			b.WriteString(text)
			b.WriteString("\n")
		}
		var turn any
		params := map[string]any{"threadId": s.thread, "input": []any{map[string]any{"type": "text", "text": b.String(), "text_elements": []any{}}}}
		if effort != "" {
			params["effort"] = effort
		}
		if e = s.p.call(ctx, "turn/start", params, &turn); e != nil {
			return result, e
		}
		s.seen = len(messages)
	}
	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-s.p.done:
			return result, errors.New("codex app-server exited during turn")
		case event := <-s.p.events:
			if len(event.ID) > 0 {
				if event.Method != "item/tool/call" {
					_ = s.p.deny(event)
					return result, fmt.Errorf("codex forbidden server action %s", event.Method)
				}
				var call struct {
					ThreadID  string
					CallID    string
					Tool      string
					Arguments json.RawMessage
				}
				if e = json.Unmarshal(event.Params, &call); e != nil {
					return result, e
				}
				allowed := false
				for _, spec := range tools {
					if spec.(map[string]any)["name"] == call.Tool {
						allowed = true
						break
					}
				}
				if call.ThreadID != s.thread || call.CallID == "" || !allowed {
					_ = s.p.deny(event)
					return result, errors.New("codex invalid or undeclared dynamic tool request")
				}
				s.pending = &event
				s.callID = call.CallID
				result.ToolCalls = []ToolCall{{ID: call.CallID, Name: call.Tool, Arguments: call.Arguments}}
				s.seen++
				return result, nil
			}
			switch event.Method {
			case "item/agentMessage/delta":
				var n struct{ Delta string }
				if e = json.Unmarshal(event.Params, &n); e != nil {
					return result, e
				}
				result.Text += n.Delta
				if onText != nil {
					if e = onText(n.Delta); e != nil {
						return result, e
					}
				}
			case "thread/tokenUsage/updated":
				var n struct {
					TokenUsage struct {
						Last struct{ InputTokens, OutputTokens, CachedInputTokens int }
					}
				}
				if json.Unmarshal(event.Params, &n) == nil {
					v := n.TokenUsage.Last
					result.Usage.InputTokens = max(0, v.InputTokens-v.CachedInputTokens)
					result.Usage.CacheReadTokens = v.CachedInputTokens
					result.Usage.OutputTokens = v.OutputTokens
					result.HasUsage = true
				}
			case "turn/completed":
				var n struct {
					Turn struct {
						Status string
						Error  *struct{ Message string }
					}
				}
				if e = json.Unmarshal(event.Params, &n); e != nil {
					return result, e
				}
				if n.Turn.Status != "completed" {
					return result, fmt.Errorf("codex turn ended with status %s", n.Turn.Status)
				}
				s.seen++
				return result, nil
			}
		}
	}
}
