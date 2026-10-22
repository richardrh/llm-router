package webui

import (
	"fmt"
	"strconv"
	"time"
)

func number(n int64) string    { return strconv.FormatInt(n, 10) }
func integer(n int) string     { return strconv.Itoa(n) }
func dollars(n float64) string { return fmt.Sprintf("$%.6f", n) }

func tokenLimit(n int) string {
	if n <= 0 {
		return "Not specified"
	}
	return strconv.Itoa(n)
}

func timestamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}
func tokenCount(n *int) string {
	if n == nil {
		return "Unknown"
	}
	return strconv.Itoa(*n)
}
func supportedAlias(a Alias) bool {
	return a.Protocol == "openai-completions" || a.Protocol == "anthropic-messages"
}
func cliAliases(aliases []Alias) bool {
	for _, a := range aliases {
		if a.HasCLI {
			return true
		}
	}
	return false
}

func enabledLabel(enabled bool) string {
	if enabled {
		return "Enabled"
	}
	return "Disabled"
}
func streamLabel(streamed bool) string {
	if streamed {
		return "Streamed"
	}
	return "Non-streaming"
}
func costLabel(cost float64, source string) string {
	if source == "reported" || source == "estimated" {
		return dollars(cost)
	}
	return "Unknown"
}
func aliasLabel(a Alias) string {
	label := a.Name + " · " + a.Protocol
	if !supportedAlias(a) {
		label += " (unsupported in playground)"
	}
	return label
}

func sessionsLabel(u Upstream) string {
	if u.Sessions == 0 && u.MaxSessions == 0 {
		return "Not applicable"
	}
	return integer(u.Sessions) + " / " + integer(u.MaxSessions)
}
func weightLabel(weight int) string {
	if weight == 0 {
		return "0 · Failover only"
	}
	return integer(weight)
}
