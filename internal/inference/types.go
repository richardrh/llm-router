// Package inference adapts the router's existing inference handler to typed
// events for in-process consumers. It does not own routing or provider execution.
package inference

import (
	"context"
	"encoding/json"

	"llm-router/internal/config"
	"llm-router/internal/usage"
)

const MaxResponseBytes = 1 << 20

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Model           string
	Protocol        config.APIProtocol
	ConversationID  string
	Messages        []Message
	Stream          bool
	MaxOutputTokens int
}

type Route struct{ Upstream, Model string }

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type EventKind uint8

const (
	RouteSelected EventKind = iota
	TextDelta
	ToolRequested
	UsageReported
)

// An event carries only the payload selected by Kind. Tool and usage events are
// complete normalized values; consumers never decode provider wire fragments.
type Event struct {
	Kind     EventKind
	Route    Route
	Text     string
	ToolCall ToolCall
	Usage    usage.Usage
}

type Result struct {
	Route     Route
	Text      string
	ToolCalls []ToolCall
	Usage     usage.Usage
	HasUsage  bool
}

type Emit func(Event) error

type Run func(context.Context, Request, Emit) (Result, error)
