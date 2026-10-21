package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("codex RPC %d: %s", e.Code, e.Message) }

type process struct {
	cmd        *exec.Cmd
	in         io.WriteCloser
	mu         sync.Mutex
	next       int
	replies    map[string]chan message
	events     chan message
	done       chan struct{}
	once       sync.Once
	loginMu    sync.Mutex
	login      *Login
	loginError string
	loginEpoch uint64
}

func startProcess(ctx context.Context, command []string, cwd string) (*process, error) {
	if len(command) == 0 {
		command = []string{"codex", "app-server", "--listen", "stdio://"}
	}
	args := append([]string{}, command[1:]...)
	args = append(args, "-c", "model_provider=\"openai\"", "-c", "forced_login_method=\"chatgpt\"", "-c", "features.shell_tool=false", "-c", "web_search=\"disabled\"", "-c", "tools.view_image=false", "-c", "features.hooks=false", "-c", "notify=[]", "-c", "features.multi_agent=false", "-c", "agents.enabled=false")
	cmd := exec.Command(command[0], args...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if k == "OPENAI_API_KEY" || k == "OPENAI_BASE_URL" || k == "CODEX_API_KEY" {
			continue
		}
		cmd.Env = append(cmd.Env, v)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	p := &process{cmd: cmd, in: in, replies: map[string]chan message{}, events: make(chan message, 256), done: make(chan struct{})}
	stopWatch := p.watch(ctx)
	defer stopWatch()
	go func() {
		defer close(p.done)
		defer cmd.Wait()
		defer p.close()
		s := bufio.NewScanner(out)
		s.Buffer(make([]byte, 4096), 16<<20)
		for s.Scan() {
			var m message
			if json.Unmarshal(s.Bytes(), &m) != nil {
				break
			}
			if m.Method == "account/login/completed" {
				var n struct {
					Success bool
					Error   string
				}
				_ = json.Unmarshal(m.Params, &n)
				p.loginMu.Lock()
				p.loginEpoch++
				p.login = nil
				p.loginError = n.Error
				p.loginMu.Unlock()
			}
			if m.Method == "" && len(m.ID) > 0 {
				p.mu.Lock()
				ch := p.replies[string(m.ID)]
				p.mu.Unlock()
				if ch != nil {
					select {
					case ch <- m:
					default:
					}
				}
				continue
			}
			select {
			case p.events <- m:
			default:
				return
			}
		}
	}()
	var init any
	if err = p.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "llm_router", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}, &init); err != nil {
		p.close()
		return nil, err
	}
	if err = p.send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}
func (p *process) close() {
	p.once.Do(func() {
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = p.in.Close()
	})
}
func (p *process) watch(ctx context.Context) func() {
	stop := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-ctx.Done():
			p.close()
		case <-stop:
		}
	}()
	return func() { close(stop); <-joined }
}
func (p *process) send(v any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	b = append(b, '\n')
	_, e = p.in.Write(b)
	return e
}
func (p *process) call(ctx context.Context, method string, params any, out any) error {
	stopWatch := p.watch(ctx)
	defer stopWatch()
	if e := ctx.Err(); e != nil {
		return e
	}
	p.mu.Lock()
	p.next++
	id := p.next
	key := fmt.Sprint(id)
	ch := make(chan message, 1)
	p.replies[key] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.replies, key); p.mu.Unlock() }()
	if e := p.send(map[string]any{"id": id, "method": method, "params": params}); e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return e
	}
	select {
	case <-ctx.Done():
		p.close()
		return ctx.Err()
	case <-p.done:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("codex app-server exited")
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	}
}
func (p *process) deny(m message) error {
	return p.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "Router forbids host actions and interactive requests"}})
}
