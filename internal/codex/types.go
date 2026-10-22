// Package codex integrates the official subscription-authenticated Codex app-server.
package codex

import (
	"encoding/json"
	"llm-router/internal/usage"
	"time"
)

type Result struct {
	Text      string
	ToolCalls []ToolCall
	Usage     usage.Usage
	HasUsage  bool
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type Login struct {
	ID       string
	URL      string
	UserCode string
}

type Account struct {
	CheckedAt time.Time
	UpdatedAt time.Time
	Models    []Model
	Type      string
	Plan      string
	SignedIn  bool
	Login     *Login
	Error     string
}

type Model struct {
	ID               string
	Name             string
	Default          bool
	ReasoningEfforts []string
	Input            []string
}
