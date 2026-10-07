package router

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/richardrh/llm-router/internal/usage"
)

func seedUsage(t *testing.T, path string, recs ...UsageRecord) *Store {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := usage.OpenStore(usage.StoreOptions{Path: path}, logger)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	for _, r := range recs {
		s.Record(r)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := usage.OpenStore(usage.StoreOptions{Path: path}, logger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func usageServer(apiKey string, store *Store) *Server {
	return &Server{cfg: &Config{APIKey: apiKey}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: store}
}

func decodeUsage(t *testing.T, res *http.Response) usageResponse {
	t.Helper()
	defer res.Body.Close()
	var out usageResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode usage response: %v", err)
	}
	return out
}

func TestUsageEndpointServesReports(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := seedUsage(t, filepath.Join(t.TempDir(), "usage.db"),
		UsageRecord{Timestamp: base, RequestID: "r1", Alias: "claude", Upstream: "openrouter", UpstreamModel: "anthropic/x", Status: 200, Attempts: 1, Streamed: true, InputTokens: new(1000), OutputTokens: new(200), CostUSD: 0.02, CostSource: "estimated"},
		UsageRecord{Timestamp: base.Add(time.Second), RequestID: "r2", Alias: "gpt", Upstream: "openai", UpstreamModel: "gpt/y", Status: 200, Attempts: 1, CostSource: "unreported"},
	)
	srv := usageServer("", store)

	res := get(t, srv, "/usage", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /usage = %d, want 200", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	body := decodeUsage(t, res)
	if len(body.Requests) != 2 || body.Requests[0].RequestID != "r2" {
		t.Fatalf("requests = %+v, want newest r2 and two rows", body.Requests)
	}
	if body.Totals.Requests != 2 || body.Totals.InputTokens != 1000 || body.Totals.CostUSD != 0.02 || body.Totals.Unreported != 1 {
		t.Errorf("totals = %+v", body.Totals)
	}
	if body.Store.Rows != 2 || body.Store.SizeBytes <= 0 {
		t.Errorf("store = %+v", body.Store)
	}

	res = get(t, srv, "/usage?group_by=alias", nil)
	grouped := decodeUsage(t, res)
	if grouped.GroupBy != "alias" || len(grouped.Groups) != 2 || len(grouped.Requests) != 0 {
		t.Fatalf("grouped = %+v", grouped)
	}

	res = get(t, srv, "/usage?alias=claude&limit=1", nil)
	filtered := decodeUsage(t, res)
	if len(filtered.Requests) != 1 || filtered.Requests[0].Alias != "claude" || filtered.Totals.Requests != 1 {
		t.Fatalf("filtered = %+v", filtered)
	}
}

func TestUsageEndpointGuards(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := seedUsage(t, filepath.Join(t.TempDir(), "usage.db"), UsageRecord{Timestamp: base, RequestID: "r1", Alias: "a", Upstream: "u", UpstreamModel: "m", CostSource: "unreported"})

	srv := usageServer("secret", store)
	if res := get(t, srv, "/usage", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /usage = %d, want 401", res.StatusCode)
	}
	if res := get(t, srv, "/usage", map[string]string{"Authorization": "Bearer secret"}); res.StatusCode != http.StatusOK {
		t.Fatalf("authenticated /usage = %d, want 200", res.StatusCode)
	}

	if res := get(t, usageServer("", nil), "/usage", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled /usage = %d, want 404", res.StatusCode)
	}
	for _, path := range []string{"/usage?group_by=password", "/usage?since=yesterday", "/usage?until=not-a-time", "/usage?limit=0", "/usage?limit=abc"} {
		if res := get(t, usageServer("", store), path, nil); res.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, res.StatusCode)
		}
	}
	if res := post(t, usageServer("", store), "/usage", map[string]any{}, nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /usage = %d, want 405", res.StatusCode)
	}
}
