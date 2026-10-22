package router

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"llm-router/internal/webui"
)

// uiStatus copies only operational fields. Config headers, keys, commands, paths,
// base URLs and conversation identities never cross this reporting seam.
func (s *Server) uiStatus() webui.Status {
	now := time.Now()
	status := webui.Status{At: now, UsageEnabled: s.store != nil}
	for _, name := range s.cfg.UpstreamsInOrder() {
		u := s.router.upstreams[name]
		item := webui.Upstream{Name: name, Kind: string(u.cfg.KindValue()), InFlight: len(u.sem), Concurrency: cap(u.sem), State: "available to attempt"}
		if u.cfg.KindValue() == UpstreamCLI {
			item.Mode = string(u.cfg.CLIModeValue())
			item.ToolMode = string(u.cfg.ToolMode)
			if item.ToolMode == "" {
				item.ToolMode = "claude"
			}
		}
		if u.codex != nil {
			item.Mode = "persistent"
			item.ToolMode = "client"
			item.Sessions, item.MaxSessions = u.codex.Counts()
		}
		u.mu.Lock()
		item.CooldownUntil = u.openUntil
		switch {
		case now.Before(u.openUntil):
			item.State = "cooling down"
		case u.probeRunning:
			item.State = "recovery probe running"
		case u.consecFails > 0 && !u.openUntil.IsZero():
			item.State = "awaiting recovery probe"
		}
		u.mu.Unlock()
		if item.State == "available to attempt" && item.InFlight >= item.Concurrency {
			item.State = "at concurrency limit"
		}
		if u.sessions != nil {
			item.Sessions, item.MaxSessions = u.sessions.SessionCounts()
		}
		status.Upstreams = append(status.Upstreams, item)
	}
	for _, name := range s.aliasNames() {
		alias := s.cfg.Models[name]
		item := webui.Alias{Name: name, Protocol: string(alias.API), ContextWindow: alias.ContextWindow, MaxOutputTokens: alias.MaxOutputTokens, Sticky: s.cfg.StickyFor(&alias)}
		for _, target := range alias.Targets {
			model, protocol := target.Model, target.API
			if model == "" {
				model = name
			}
			if protocol == "" {
				protocol = alias.API
			}
			item.Targets = append(item.Targets, webui.Target{Upstream: target.Upstream, Model: model, Protocol: string(protocol), Weight: target.WeightValue()})
			up := s.cfg.Upstreams[target.Upstream]
			if up.KindValue() == UpstreamCLI {
				item.HasCLI = true
				if up.ToolMode == CLIToolsClient {
					item.ClientTools = true
				}
			}
		}
		status.Aliases = append(status.Aliases, item)
	}
	return status
}

func (s *Server) uiUsage(q url.Values) (webui.UsageData, error) {
	data := webui.UsageData{Enabled: s.store != nil, Since: q.Get("since"), Until: q.Get("until"), Alias: q.Get("alias"), Upstream: q.Get("upstream"), GroupBy: q.Get("group_by"), Limit: q.Get("limit")}
	if !data.Enabled {
		return data, nil
	}
	report, status, err := s.queryUsage(q)
	if err != nil {
		if status == http.StatusInternalServerError {
			// SQLite errors may contain filesystem paths; retain them only in server logs.
			s.log.Error("console usage query failed", "error", err)
			return data, fmt.Errorf("usage history is temporarily unavailable")
		}
		return data, err
	}
	data.Totals, data.Groups, data.Requests = report.Totals, report.Groups, report.Requests
	data.Dropped, data.Store = report.DroppedRecords, report.Store
	return data, nil
}
