// Package webui serves the embedded operational console without application JavaScript.
package webui

import (
	"context"
	"net/url"
	"time"

	"llm-router/internal/inference"
	"llm-router/internal/usage"
)

// Dependencies keeps routing and accounting owned by their existing modules.
// Execution is typed; HTTP and provider wire parsing stay outside the web module.
type Dependencies struct {
	GatewayKey string
	// SecureCookies is set explicitly when HTTPS terminates at a trusted reverse proxy.
	SecureCookies bool
	Snapshot      func() Status
	Usage         func(url.Values) (UsageData, error)
	Run           inference.Run
	Connections   func() []Connection
	Connect       func(context.Context, ConnectionAction) error
}

type Status struct {
	At           time.Time
	Upstreams    []Upstream
	Aliases      []Alias
	UsageEnabled bool
}

type Upstream struct {
	Name          string
	Kind          string
	Mode          string
	ToolMode      string
	InFlight      int
	Concurrency   int
	State         string
	CooldownUntil time.Time
	Sessions      int
	MaxSessions   int
}

type Alias struct {
	Name            string
	Protocol        string
	ContextWindow   int
	MaxOutputTokens int
	Sticky          bool
	Targets         []Target
	// Any CLI target requires acknowledgement, even if it is a backup.
	HasCLI bool
	// Any client-tool target makes playground requests non-streaming.
	ClientTools bool
}

type Target struct {
	Upstream string
	Model    string
	Protocol string
	Weight   int
}

type UsageData struct {
	Enabled  bool
	Since    string
	Until    string
	Alias    string
	Upstream string
	GroupBy  string
	Limit    string
	Totals   usage.UsageTotals
	Groups   []usage.UsageGroup
	Requests []usage.UsageRecord
	Dropped  uint64
	Store    usage.StoreStats
	Error    string
}

type PlayView struct {
	CSRF           string
	ConversationID string
	Alias          string
	Aliases        []Alias
	Messages       []MessageView
	State          ConversationState
	Revision       uint64
	ActiveStart    int
	Error          string
	Upstream       string
	UpstreamModel  string
	StreamURL      string
}

type MessageView struct {
	Role string
	Text string
}

// Connection is an allowlisted provider-account view, never a credential payload.
type Connection struct {
	CheckedAt   time.Time
	UpdatedAt   time.Time
	Name        string
	Kind        string
	AccountType string
	Plan        string
	SignedIn    bool
	LoginID     string
	LoginURL    string
	UserCode    string
	Error       string
	Models      []ConnectionModel
}

type ConnectionModel struct {
	ID      string
	Name    string
	Default bool
}

type ConnectionActionKind string

const (
	ConnectionRefresh ConnectionActionKind = "refresh"
	ConnectionLogin   ConnectionActionKind = "login"
	ConnectionCancel  ConnectionActionKind = "cancel"
	ConnectionLogout  ConnectionActionKind = "logout"
)

type ConnectionAction struct {
	Upstream string
	Kind     ConnectionActionKind
	LoginID  string
}
