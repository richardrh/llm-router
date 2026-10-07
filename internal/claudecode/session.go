package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"llm-router/internal/config"
)

var (
	errCLISessionBusy = errors.New("CLI session limit reached")
	errCLISessionLost = errors.New("CLI session process exited")
)

type cliSessionManager struct {
	mu          sync.Mutex
	sessions    map[string]*cliSession
	resume      map[string]string
	cfg         Upstream
	maxSessions int
	idleTimeout time.Duration
}

func newCLISessionManager(cfg Upstream, maxSessions int, idleTimeout time.Duration) *cliSessionManager {
	if maxSessions < 1 {
		maxSessions = 1
	}
	return &cliSessionManager{
		sessions:    make(map[string]*cliSession),
		resume:      make(map[string]string),
		cfg:         cfg,
		maxSessions: maxSessions,
		idleTimeout: idleTimeout,
	}
}

type cliSession struct {
	key   string
	model string
	cfg   Upstream

	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	reader    *bufio.Reader
	stderr    *cliStderr
	done      chan error
	turnMu    sync.Mutex
	mu        sync.Mutex
	last      time.Time
	closed    bool
	turns     int
	sessionID string
}

func (m *cliSessionManager) run(ctx context.Context, key, model, firstPrompt, nextPrompt string, onText func(string) error) (cliRun, error) {
	session, err := m.session(key, model)
	if err != nil {
		return cliRun{}, err
	}
	run, err := session.run(ctx, firstPrompt, nextPrompt, onText)
	if err != nil && errors.Is(err, errCLISessionLost) {
		m.remove(key, session)
	}
	return run, err
}

func (m *cliSessionManager) session(key, model string) (*cliSession, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("persistent CLI upstream requires a session key")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.sessions[key]; ok {
		if m.idleTimeout > 0 && time.Since(session.touched()) > m.idleTimeout {
			delete(m.sessions, key)
			if id := session.id(); id != "" {
				m.resume[key] = id
			}
			session.close()
		} else if session.model != model {
			return nil, fmt.Errorf("CLI session %q is already bound to model %q", key, session.model)
		} else {
			return session, nil
		}
	}
	if len(m.sessions) >= m.maxSessions {
		return nil, errCLISessionBusy
	}
	resumeID := m.resume[key]
	session, err := startCLISession(key, model, m.cfg, resumeID)
	if err != nil {
		return nil, err
	}
	delete(m.resume, key)
	m.sessions[key] = session
	return session, nil
}

func (m *cliSessionManager) remove(key string, session *cliSession) {
	m.mu.Lock()
	if current, ok := m.sessions[key]; ok && current == session {
		delete(m.sessions, key)
		if id := session.id(); id != "" {
			m.resume[key] = id
		}
	}
	m.mu.Unlock()
	session.close()
}

func (m *cliSessionManager) closeAll() {
	m.mu.Lock()
	sessions := make([]*cliSession, 0, len(m.sessions))
	for key, session := range m.sessions {
		delete(m.sessions, key)
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		session.close()
	}
}

func startCLISession(key, model string, cfg Upstream, resumeID string) (*cliSession, error) {
	argv, err := buildCLISessionCommand(cfg.Command, model, resumeID, cfg.ToolMode)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cfg.WorkingDirectory != "" {
		cmd.Dir = cfg.WorkingDirectory
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("capture CLI stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("capture CLI stdout: %w", err)
	}
	stderr := &cliStderr{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, cliCommandError("start CLI command", err, stderr.String())
	}

	session := &cliSession{
		key:    key,
		model:  model,
		cfg:    cfg,
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		reader: bufio.NewReader(stdout),
		stderr: stderr,
		done:   make(chan error, 1),
		last:   time.Now(),
	}
	go func() { session.done <- cmd.Wait() }()
	return session, nil
}

func buildCLISessionCommand(argv []string, model, resumeID string, toolMode config.CLIToolMode) ([]string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("CLI command is empty")
	}
	for _, arg := range argv {
		if strings.Contains(arg, "{prompt}") {
			return nil, errors.New("persistent CLI command must not contain the {prompt} placeholder")
		}
	}
	out := append([]string(nil), argv...)
	for i, arg := range out {
		if strings.Contains(arg, "{model}") {
			out[i] = strings.ReplaceAll(arg, "{model}", model)
		}
	}
	if toolMode == config.CLIToolsClient {
		out = append(out, "--tools", "")
	}
	if resumeID != "" {
		out = append(out, "--resume", resumeID)
	}
	return out, nil
}

func (s *cliSession) touched() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *cliSession) id() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *cliSession) touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

func (s *cliSession) run(ctx context.Context, firstPrompt, nextPrompt string, onText func(string) error) (cliRun, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	s.touch()

	s.mu.Lock()
	prompt := nextPrompt
	if s.turns == 0 {
		prompt = firstPrompt
	}
	s.mu.Unlock()
	message := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": prompt,
		},
		"parent_tool_use_id": nil,
	}
	body, err := json.Marshal(message)
	if err != nil {
		return cliRun{}, err
	}
	if _, err := s.stdin.Write(append(body, '\n')); err != nil {
		return cliRun{}, cliCommandError("write CLI input", err, s.stderr.String())
	}

	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killCLIProcess(s.cmd)
		case <-cancelDone:
		case <-s.done:
		}
	}()
	defer close(cancelDone)

	var result cliRun
	seenResult := false
	reader := s.reader
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var event cliEvent
			if json.Unmarshal(line, &event) == nil {
				if event.SessionID != "" {
					s.mu.Lock()
					s.sessionID = event.SessionID
					s.mu.Unlock()
				}
				if err := event.apply(&result, &seenResult, onText); err != nil {
					return cliRun{}, err
				}
				if event.Type == "result" && s.cfg.ToolMode == config.CLIToolsClient {
					result.ToolCall, result.Text = parseClientEnvelope(result.Text)
				}
			}
		}
		if seenResult {
			s.mu.Lock()
			s.turns++
			s.last = time.Now()
			s.mu.Unlock()
			return result, nil
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return cliRun{}, ctx.Err()
			}
			if errors.Is(readErr, io.EOF) {
				return cliRun{}, cliCommandError("read CLI output", errCLISessionLost, s.stderr.String())
			}
			return cliRun{}, cliCommandError("read CLI output", readErr, s.stderr.String())
		}
	}
}

func (s *cliSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	killCLIProcess(s.cmd)
	_ = s.stdin.Close()
	_ = s.stdout.Close()
	select {
	case <-s.done:
	case <-time.After(time.Second):
	}
}

// cliEvent is the subset of Claude Code's stream-json protocol needed by the
// compatibility endpoints. Unknown events are deliberately ignored so newer
// Claude Code releases remain forward-compatible.
type cliEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Event     struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	IsError   bool           `json:"is_error"`
	Result    string         `json:"result"`
	TotalCost *float64       `json:"total_cost_usd"`
	Usage     map[string]any `json:"usage"`
}

func (e cliEvent) apply(result *cliRun, seenResult *bool, onText func(string) error) error {
	switch e.Type {
	case "stream_event":
		if e.Event.Type == "content_block_delta" && e.Event.Delta.Type == "text_delta" && onText != nil {
			if err := onText(e.Event.Delta.Text); err != nil {
				return err
			}
		}
	case "result":
		*seenResult = true
		result.Text = e.Result
		if e.TotalCost != nil {
			result.CostUSD = *e.TotalCost
			result.HasCost = true
		}
		if len(e.Usage) > 0 {
			encoded, _ := json.Marshal(map[string]any{"usage": e.Usage})
			result.Usage = Usage{}
			result.Usage.MergeFromJSON(encoded)
		}
		if e.IsError {
			return errors.New("CLI returned an error result")
		}
	}
	return nil
}
