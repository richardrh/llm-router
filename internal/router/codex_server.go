package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"llm-router/internal/codex"
)

// attemptCodex commits only after actual text or a completed backend result.
// Once committed, errors are stream events, never invitations to replay a turn.
func (s *Server) attemptCodex(ctx context.Context, w http.ResponseWriter, plan *requestPlan, c candidate, logger *slog.Logger) (*http.Response, attemptOutcome, error) {
	if c.upstream.codex == nil {
		return nil, attemptOutcome{}, fmt.Errorf("codex upstream has no session manager")
	}
	body, err := plan.bodyFor(c, false, APIOpenAICompletions)
	if err != nil {
		return nil, attemptOutcome{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, attemptOutcome{}, err
	}
	timeout := c.upstream.cfg.Timeout.Duration()
	if timeout <= 0 {
		timeout = s.cfg.Defaults.MaxStreamDuration.Duration()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	outcome := attemptOutcome{status: http.StatusOK, streamed: plan.stream, costUnavailable: true}
	headers := func() {
		w.Header().Set("X-Router-Upstream", c.upstream.name)
		w.Header().Set("X-Router-Upstream-Model", c.target.Model)
	}
	if !plan.stream {
		result, err := c.upstream.codex.Run(ctx, plan.alias+"\x00"+plan.sessionKey, c.target.Model, fields, nil)
		if err != nil {
			return nil, attemptOutcome{}, err
		}
		outcome.usage, outcome.hasUsage = result.Usage, result.HasUsage
		response, err := codexResponse(result, plan.alias)
		if err != nil {
			return nil, attemptOutcome{}, err
		}
		headers()
		w.Header().Set("Content-Type", "application/json")
		n, err := w.Write(response)
		outcome.bytes = int64(n)
		if err != nil {
			logger.Warn("codex client write failed", "error", err)
		}
		return nil, outcome, nil
	}
	committed := false
	raw := func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		if !committed {
			headers()
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			committed = true
		}
		n, err := w.Write(b)
		outcome.bytes += int64(n)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return err
	}
	emit := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		b = append(append([]byte("data: "), b...), '\n', '\n')
		return raw(b)
	}
	chunk := func(delta any, finish any, usage any) map[string]any {
		v := map[string]any{"id": "chatcmpl-codex", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": plan.alias, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			v["usage"] = usage
		}
		return v
	}
	result, runErr := c.upstream.codex.Run(ctx, plan.alias+"\x00"+plan.sessionKey, c.target.Model, fields, func(text string) error {
		if text == "" {
			return nil
		}
		return emit(chunk(map[string]any{"role": "assistant", "content": text}, nil, nil))
	})
	if runErr != nil {
		if !committed {
			return nil, attemptOutcome{}, runErr
		}
		outcome.status = http.StatusBadGateway
		logger.Error("codex upstream failed mid-stream", "upstream", c.upstream.name, "error", runErr)
		// The response is committed: emit an explicit error, never replay.
		b, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "upstream_error", "message": runErr.Error()}})
		_ = raw(append(append([]byte("data: "), b...), '\n', '\n'))
		_ = raw([]byte("data: [DONE]\n\n"))
		return nil, outcome, nil
	}
	outcome.usage, outcome.hasUsage = result.Usage, result.HasUsage
	delta := map[string]any{}
	finish := "stop"
	if len(result.ToolCalls) > 0 {
		delta["tool_calls"] = codexToolCalls(result.ToolCalls, true)
		finish = "tool_calls"
	}
	var usage any
	if result.HasUsage {
		usage = cliOpenAIUsage(result.Usage)
	}
	if err := emit(chunk(delta, finish, usage)); err != nil {
		// A completed backend turn must not be replayed on another upstream even if
		// the client disconnects during its first write.
		outcome.status = http.StatusBadGateway
		return nil, outcome, nil
	}
	_ = raw([]byte("data: [DONE]\n\n"))
	return nil, outcome, nil
}

func codexToolCalls(calls []codex.ToolCall, indexed bool) []any {
	out := make([]any, 0, len(calls))
	for i, call := range calls {
		v := map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": string(call.Arguments)}}
		if indexed {
			v["index"] = i
		}
		out = append(out, v)
	}
	return out
}

func codexResponse(result codex.Result, alias string) ([]byte, error) {
	message := map[string]any{"role": "assistant", "content": result.Text}
	finish := "stop"
	if len(result.ToolCalls) > 0 {
		message["tool_calls"] = codexToolCalls(result.ToolCalls, false)
		finish = "tool_calls"
	}
	response := map[string]any{"id": "chatcmpl-codex", "object": "chat.completion", "created": time.Now().Unix(), "model": alias, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}}
	if result.HasUsage {
		response["usage"] = cliOpenAIUsage(result.Usage)
	}
	return json.Marshal(response)
}
