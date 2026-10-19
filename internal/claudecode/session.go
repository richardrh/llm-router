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
	errCLISessionBusy   = errors.New("CLI session limit reached")
	errCLISessionLost   = errors.New("CLI session process exited")
	errCLISessionClosed = errors.New("CLI session was retired before the turn began")
)

type cliSessionManager struct {
	mu          sync.Mutex
	sessions    map[string]*cliSession
	resume      map[string]string
	cfg         Upstream
	maxSessions int
	idleTimeout time.Duration
	generation  uint64
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
	stderr    *cliSessionStderr
	done      chan struct{}
	closeDone chan struct{}
	turn      chan struct{}
	mu        sync.Mutex
	last      time.Time
	closed    bool
	turns     int
	sessionID string
	// users includes active and queued turns and is protected by the manager's mutex.
	users int
}

type cliSessionStderr struct {
	mu  sync.Mutex
	log cliStderr
}

func (s *cliSessionStderr) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.log.Write(p)
}

func (s *cliSessionStderr) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.log.String()
}

func (m *cliSessionManager) run(ctx context.Context, key, model, firstPrompt, nextPrompt string, onText func(string) error) (cliRun, error) {
	m.mu.Lock()
	generation := m.generation
	m.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return cliRun{}, err
		}
		session, err := m.session(key, model, generation)
		if err != nil {
			return cliRun{}, err
		}
		run, err := session.run(ctx, firstPrompt, nextPrompt, onText)
		if session.isClosed() {
			m.remove(key, session)
		}
		m.mu.Lock()
		session.users--
		session.touch()
		m.mu.Unlock()
		if errors.Is(err, errCLISessionClosed) {
			continue
		}
		return run, err
	}
}

func (m *cliSessionManager) session(key, model string, generation uint64) (*cliSession, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("persistent CLI upstream requires a session key")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.generation {
		return nil, errCLISessionLost
	}
	for existingKey, session := range m.sessions {
		if session.isClosed() || (session.users == 0 && m.idleTimeout > 0 && time.Since(session.touched()) > m.idleTimeout) {
			delete(m.sessions, existingKey)
			if id := session.id(); id != "" {
				m.resume[existingKey] = id
			}
			session.close()
		}
	}
	if session, ok := m.sessions[key]; ok {
		if session.model != model {
			return nil, fmt.Errorf("CLI session %q is already bound to model %q", key, session.model)
		}
		session.users++
		return session, nil
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
	session.users = 1
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
	m.generation++
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
	stderr := &cliSessionStderr{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, cliCommandError("start CLI command", err, stderr.String())
	}

	session := &cliSession{
		key:       key,
		model:     model,
		cfg:       cfg,
		cmd:       cmd,
		stdin:     stdin,
		stdout:    stdout,
		reader:    bufio.NewReader(stdout),
		stderr:    stderr,
		done:      make(chan struct{}),
		closeDone: make(chan struct{}),
		turn:      make(chan struct{}, 1),
		last:      time.Now(),
		sessionID: resumeID,
	}
	if resumeID != "" {
		session.turns = 1
	}
	go func() {
		_ = cmd.Wait()
		close(session.done)
	}()
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

func (s *cliSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *cliSession) touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

func (s *cliSession) run(ctx context.Context, firstPrompt, nextPrompt string, onText func(string) error) (result cliRun, err error) {
	select {
	case s.turn <- struct{}{}:
		defer func() { <-s.turn }()
	case <-ctx.Done():
		return cliRun{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return cliRun{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return cliRun{}, errCLISessionClosed
	}
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
	cancelStop := make(chan struct{})
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		select {
		case <-ctx.Done():
			s.close()
		case <-cancelStop:
		case <-s.done:
		}
	}()
	incomplete := true
	defer func() {
		close(cancelStop)
		<-cancelDone
		if ctx.Err() != nil {
			result, err = cliRun{}, ctx.Err()
			incomplete = true
		}
		if incomplete {
			s.close()
		}
	}()
	if _, err := s.stdin.Write(append(body, '\n')); err != nil {
		return cliRun{}, cliCommandError("write CLI input", err, s.stderr.String())
	}

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
				applyErr := event.apply(&result, &seenResult, onText)
				if seenResult {
					s.mu.Lock()
					s.turns++
					s.mu.Unlock()
					incomplete = false
					if applyErr != nil {
						return cliRun{}, applyErr
					}
					if s.cfg.ToolMode == config.CLIToolsClient {
						result.ToolCall, result.Text = parseClientEnvelope(result.Text)
					}
					return result, nil
				}
				if applyErr != nil {
					return cliRun{}, applyErr
				}
			}
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
		<-s.closeDone
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
	close(s.closeDone)
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
