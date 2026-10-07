package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"llm-router/internal/claudecode"
	"llm-router/internal/config"
	"llm-router/internal/usage"
	"llm-router/internal/wire"
)

type Config = config.Config
type Defaults = config.Defaults
type Duration = config.Duration
type APIProtocol = config.APIProtocol
type StoreConfig = config.StoreConfig
type Upstream = config.Upstream
type UpstreamKind = config.UpstreamKind
type CLIMode = config.CLIMode
type CLIToolMode = config.CLIToolMode
type Alias = config.Alias
type Target = config.Target
type Cost = config.Cost

type Usage = usage.Usage
type UsageRecord = usage.UsageRecord
type UsageFilter = usage.UsageFilter
type UsageTotals = usage.UsageTotals
type UsageGroup = usage.UsageGroup
type StoreStats = usage.StoreStats
type Store = usage.Store
type usageObserver = usage.Observer

type capabilityTable = config.CapabilityTable
type ToolCall = claudecode.ToolCall
type cliRun = claudecode.Run
type cliSessionManager = claudecode.SessionManager
type streamTranslator = wire.StreamTranslator

type APIModel = wire.APIProtocol

const (
	APIOpenAICompletions = config.APIOpenAICompletions
	APIOpenAIResponses   = config.APIOpenAIResponses
	APIAnthropicMessages = config.APIAnthropicMessages
	AuthBearer           = config.AuthBearer
	AuthAnthropic        = config.AuthAnthropic
	UpstreamHTTP         = config.UpstreamHTTP
	UpstreamCLI          = config.UpstreamCLI
	CLIOneShot           = config.CLIOneShot
	CLIPersistent        = config.CLIPersistent
	CLIToolsClaude       = config.CLIToolsClaude
	CLIToolsClient       = config.CLIToolsClient
)

func LoadConfig(path string) (*Config, error) { return config.LoadConfig(path) }
func openUsageStore(cfg *Config, log *slog.Logger) (*Store, error) {
	return usage.OpenStoreForConfig(cfg, log)
}
func newCLISessionManager(cfg Upstream, maxSessions int, idleTimeout time.Duration) *cliSessionManager {
	return claudecode.NewSessionManager(cfg, maxSessions, idleTimeout)
}
func buildCLICommand(argv []string, prompt string) ([]string, error) {
	return claudecode.BuildCommand(argv, prompt)
}
func newCLIPrompt(fields map[string]json.RawMessage) (string, error) {
	return claudecode.BuildPrompt(fields)
}
func newCLILastPrompt(fields map[string]json.RawMessage) (string, error) {
	return claudecode.BuildLastPrompt(fields)
}
func newCLIClientToolPrompt(fields map[string]json.RawMessage) (string, error) {
	return claudecode.BuildClientToolPrompt(fields)
}
func newCLIContinuationPrompt(fields map[string]json.RawMessage) (string, error) {
	return claudecode.BuildContinuationPrompt(fields)
}
func runCLICommand(ctx context.Context, argv []string, onText func(string) error) (cliRun, error) {
	return claudecode.RunCommand(ctx, argv, onText)
}
func cliOpenAIResponse(r cliRun, alias string) []byte { return claudecode.OpenAIResponse(r, alias) }
func cliAnthropicResponse(r cliRun, alias string) []byte {
	return claudecode.AnthropicResponse(r, alias)
}
func newCLIOpenAIStream(alias string) *claudecode.OpenAIStream {
	return claudecode.NewOpenAIStream(alias)
}
func cliOpenAIUsage(u Usage) map[string]interface{} {
	return claudecode.OpenAIUsage(u)
}

func translatable(client, upstream APIProtocol) bool { return wire.Translatable(client, upstream) }
func translateRequest(client, upstream APIProtocol, body []byte) ([]byte, error) {
	return wire.TranslateRequest(client, upstream, body)
}
func translateResponse(client, upstream APIProtocol, body []byte, alias string) ([]byte, error) {
	return wire.TranslateResponse(client, upstream, body, alias)
}
func newStreamTranslator(client, upstream APIProtocol, alias string) streamTranslator {
	return wire.NewStreamTranslator(client, upstream, alias)
}

func estimateCost(c Cost, u Usage) float64 { return usage.Estimate(c, u) }

const (
	flagVision           = config.FlagVision
	flagFunctionCalling  = config.FlagFunctionCalling
	flagReasoning        = config.FlagReasoning
	flagPromptCaching    = config.FlagPromptCaching
	flagAdaptiveThinking = config.FlagAdaptiveThinking
	flagThinkingAlwaysOn = config.FlagThinkingAlwaysOn
)

func loadCapabilities() (*capabilityTable, error)   { return config.LoadCapabilities() }
func supportedEfforts(f config.ModelFacts) []string { return config.SupportedEfforts(f) }

func normalizeModelID(id string) string { return config.NormalizeModelID(id) }

func usageFromBody(body []byte) (Usage, bool) { return usage.UsageFromBody(body) }

var errTranslationFailed = wire.ErrTranslationFailed

const defaultAnthropicVersion = config.DefaultAnthropicVersion
