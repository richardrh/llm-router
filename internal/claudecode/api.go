package claudecode

import (
	"context"
	"encoding/json"
	"time"

	"github.com/richardrh/llm-router/internal/config"
	"github.com/richardrh/llm-router/internal/usage"
)

// Run is the result of one Claude Code turn.
type Run = cliRun

// Usage is the normalized provider usage reported by Claude Code.
type Usage = usage.Usage

// Upstream is the CLI configuration consumed by the session manager.
type Upstream = config.Upstream

// SessionManager owns persistent Claude Code processes.
type SessionManager = cliSessionManager

func NewSessionManager(cfg config.Upstream, maxSessions int, idleTimeout time.Duration) *SessionManager {
	return newCLISessionManager(cfg, maxSessions, idleTimeout)
}

func (m *SessionManager) Run(ctx context.Context, key, model, firstPrompt, nextPrompt string, onText func(string) error) (Run, error) {
	return m.run(ctx, key, model, firstPrompt, nextPrompt, onText)
}

func (m *SessionManager) CloseAll() { m.closeAll() }

func RunCommand(ctx context.Context, argv []string, onText func(string) error) (Run, error) {
	return runCLICommand(ctx, argv, onText)
}

func BuildCommand(argv []string, prompt string) ([]string, error) {
	return buildCLICommand(argv, prompt)
}

func BuildPrompt(fields map[string]json.RawMessage) (string, error) {
	return newCLIPrompt(fields)
}

func BuildLastPrompt(fields map[string]json.RawMessage) (string, error) {
	return newCLILastPrompt(fields)
}

func BuildClientToolPrompt(fields map[string]json.RawMessage) (string, error) {
	return newCLIClientToolPrompt(fields)
}

func BuildContinuationPrompt(fields map[string]json.RawMessage) (string, error) {
	return newCLIContinuationPrompt(fields)
}

func OpenAIResponse(r Run, alias string) []byte    { return cliOpenAIResponse(r, alias) }
func AnthropicResponse(r Run, alias string) []byte { return cliAnthropicResponse(r, alias) }

type OpenAIStream = cliOpenAIStream

func NewOpenAIStream(alias string) *OpenAIStream { return newCLIOpenAIStream(alias) }

func OpenAIUsage(u Usage) map[string]interface{} { return cliOpenAIUsage(u) }
