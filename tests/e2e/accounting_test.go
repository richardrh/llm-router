package e2e

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type accountingTotals struct {
	Requests   int     `json:"requests"`
	Input      int     `json:"input_tokens"`
	Output     int     `json:"output_tokens"`
	Read       int     `json:"cache_read_tokens"`
	Write      int     `json:"cache_write_tokens"`
	Cost       float64 `json:"cost_usd"`
	Unreported int     `json:"unreported"`
}
type accountingRow struct {
	TS       time.Time `json:"ts"`
	Alias    string    `json:"alias"`
	Upstream string    `json:"upstream"`
	Model    string    `json:"upstream_model"`
	Input    *int      `json:"input_tokens"`
	Output   *int      `json:"output_tokens"`
	Read     *int      `json:"cache_read_tokens"`
	Write    *int      `json:"cache_write_tokens"`
	Cost     float64   `json:"cost_usd"`
	Source   string    `json:"cost_source"`
}
type accountingReport struct {
	Totals   accountingTotals `json:"totals"`
	Requests []accountingRow  `json:"requests"`
	Groups   []struct {
		Key string `json:"key"`
		accountingTotals
	} `json:"groups"`
	Dropped int `json:"dropped_records"`
}

func accountingConfig(provider, path string, maxRows int) string {
	return fmt.Sprintf(`apiKey: %q
ui: {enabled: true, secureCookies: false}
store: {path: %q, queueSize: 128, maxRows: %d}
upstreams:
  fixture: {baseUrl: %q, apiKey: synthetic-provider-key}
models:
  measured:
    api: openai-completions
    cost: {input: 4, output: 20, cacheRead: 0.2, cacheWrite: 5}
    targets: [{upstream: fixture, model: measured, weight: 1}]
  zero:
    api: openai-completions
    cost: {input: 4, output: 20, cacheRead: 0.2, cacheWrite: 5}
    targets: [{upstream: fixture, model: zero, weight: 1}]
  missing:
    api: openai-completions
    cost: {input: 4, output: 20, cacheRead: 0.2, cacheWrite: 5}
    targets: [{upstream: fixture, model: missing, weight: 1}]
  anthropic:
    api: anthropic-messages
    cost: {input: 4, output: 20, cacheRead: 0.2, cacheWrite: 5}
    targets: [{upstream: fixture, model: anthropic, api: anthropic-messages, weight: 1}]
`, gatewayKey, path, maxRows, provider)
}

func accountingProvider(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("provider request: %v", err)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if body.Model == "anthropic" {
			fmt.Fprint(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":"anthropic","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":20,"cache_read_input_tokens":80,"cache_creation_input_tokens":10}}`)
			return
		}
		usage := ""
		switch body.Model {
		case "measured":
			usage = `,"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":80}}`
		case "zero":
			usage = `,"usage":{"prompt_tokens":0,"completion_tokens":0}`
		case "missing":
		default:
			t.Errorf("unexpected model %q", body.Model)
		}
		fmt.Fprintf(w, `{"id":"chat_fixture","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]%s}`, body.Model, usage)
	}))
	t.Cleanup(s.Close)
	return s
}

func accountingInfer(t *testing.T, p *routerProcess, alias string) {
	t.Helper()
	path := "/v1/chat/completions"
	if alias == "anthropic" {
		path = "/v1/messages"
	}
	result := request(t, "POST", p.URL+path, fmt.Sprintf(`{"model":%q,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, alias), map[string]string{"Authorization": "Bearer " + gatewayKey})
	if result.Status != 200 {
		t.Fatalf("inference %s: %d %s", alias, result.Status, result.Body)
	}
}
func accountingGet(t *testing.T, p *routerProcess, query string) accountingReport {
	t.Helper()
	result := request(t, "GET", p.URL+"/usage"+query, "", map[string]string{"Authorization": "Bearer " + gatewayKey})
	if result.Status != 200 {
		t.Fatalf("usage %s: %d %s", query, result.Status, result.Body)
	}
	var report accountingReport
	if err := json.Unmarshal(result.Body, &report); err != nil {
		t.Fatal(err)
	}
	return report
}
func accountingNumber(t *testing.T, name string, got *int, want int) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want present %d", name, got, want)
	}
}
func accountingCost(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %.12f, want %.12f", got, want)
	}
}

// U1: HTTP completion is not a durability barrier. Only graceful shutdown is.
func TestAccountingHTTPUsageSurvivesGracefulRestart(t *testing.T) {
	provider := accountingProvider(t)
	config := accountingConfig(provider.URL, filepath.Join(t.TempDir(), "usage.db"), 0)
	p := startRouter(t, config)
	for _, alias := range []string{"measured", "zero", "missing"} {
		accountingInfer(t, p, alias)
	}
	p.Stop(t, syscall.SIGTERM)
	p = startRouter(t, config)
	report := accountingGet(t, p, "")
	if report.Totals.Requests != 3 || len(report.Requests) != 3 || report.Dropped != 0 {
		t.Fatalf("accepted rows not preserved: %+v", report)
	}
	rows := make(map[string]accountingRow)
	for _, row := range report.Requests {
		rows[row.Alias] = row
	}
	measured := rows["measured"]
	accountingNumber(t, "uncached input", measured.Input, 20)
	accountingNumber(t, "output", measured.Output, 20)
	accountingNumber(t, "cache read", measured.Read, 80)
	accountingCost(t, measured.Cost, .000496)
	if measured.Source != "estimated" {
		t.Fatalf("measured source: %q", measured.Source)
	}
	zero := rows["zero"]
	accountingNumber(t, "zero input", zero.Input, 0)
	accountingNumber(t, "zero output", zero.Output, 0)
	accountingCost(t, zero.Cost, 0)
	if zero.Source != "estimated" {
		t.Fatalf("explicit zero source: %q", zero.Source)
	}
	missing := rows["missing"]
	if missing.Input != nil || missing.Output != nil || missing.Read != nil || missing.Write != nil || missing.Source != "unreported" {
		t.Fatalf("missing usage became measured: %+v", missing)
	}
	accountingCost(t, missing.Cost, 0)
	if report.Totals.Input != 20 || report.Totals.Output != 20 || report.Totals.Read != 80 || report.Totals.Unreported != 1 {
		t.Fatalf("totals: %+v", report.Totals)
	}
	accountingCost(t, report.Totals.Cost, .000496)
	page := accountingGet(t, p, "?limit=1")
	if len(page.Requests) != 1 || page.Totals.Requests != 3 {
		t.Fatalf("page limit truncated totals: %+v", page)
	}
	for _, dimension := range []string{"alias", "upstream", "upstream_model", "cost_source"} {
		grouped := accountingGet(t, p, "?group_by="+dimension)
		expected := map[string]int{"measured": 1, "zero": 1, "missing": 1}
		if dimension == "upstream" {
			expected = map[string]int{"fixture": 3}
		}
		if dimension == "cost_source" {
			expected = map[string]int{"estimated": 2, "unreported": 1}
		}
		if len(grouped.Groups) != len(expected) || grouped.Totals.Requests != 3 {
			t.Fatalf("group %s: %+v", dimension, grouped)
		}
		for _, group := range grouped.Groups {
			if want, exists := expected[group.Key]; !exists || group.Requests != want {
				t.Fatalf("group %s key %q count %d, expected %+v", dimension, group.Key, group.Requests, expected)
			}
			delete(expected, group.Key)
		}
		if len(expected) != 0 {
			t.Fatalf("missing %s groups: %+v", dimension, expected)
		}
	}
	filtered := accountingGet(t, p, "?alias=measured&upstream=fixture")
	if filtered.Totals.Requests != 1 || len(filtered.Requests) != 1 || filtered.Requests[0].Alias != "measured" {
		t.Fatalf("combined filter: %+v", filtered)
	}
	if got := accountingGet(t, p, "?alias=measured&upstream=absent"); got.Totals.Requests != 0 {
		t.Fatalf("filters are not conjunctive: %+v", got)
	}
	// Use an actual recorded boundary, not sleeps or assumptions about clock resolution.
	sort.Slice(report.Requests, func(i, j int) bool { return report.Requests[i].TS.Before(report.Requests[j].TS) })
	boundary := report.Requests[1].TS
	before, after := 0, 0
	for _, row := range report.Requests {
		if row.TS.Before(boundary) {
			before++
		} else {
			after++
		}
	}
	encoded := url.QueryEscape(boundary.Format(time.RFC3339Nano))
	if got := accountingGet(t, p, "?until="+encoded); got.Totals.Requests != before {
		t.Fatalf("exclusive until: %+v, want %d", got, before)
	}
	if got := accountingGet(t, p, "?since="+encoded); got.Totals.Requests != after {
		t.Fatalf("inclusive since: %+v, want %d", got, after)
	}
	if got := accountingGet(t, p, "?since="+encoded+"&until="+encoded); got.Totals.Requests != 0 {
		t.Fatalf("empty half-open window: %+v", got)
	}
}

func TestAccountingAnthropicCachePrice(t *testing.T) {
	provider := accountingProvider(t)
	config := accountingConfig(provider.URL, filepath.Join(t.TempDir(), "usage.db"), 0)
	p := startRouter(t, config)
	accountingInfer(t, p, "anthropic")
	p.Stop(t, syscall.SIGTERM)
	p = startRouter(t, config)
	report := accountingGet(t, p, "")
	if len(report.Requests) != 1 {
		t.Fatalf("rows: %+v", report)
	}
	row := report.Requests[0]
	accountingNumber(t, "input", row.Input, 20)
	accountingNumber(t, "output", row.Output, 20)
	accountingNumber(t, "cache read", row.Read, 80)
	accountingNumber(t, "cache write", row.Write, 10)
	// 20*4 + 20*20 + 80*.2 + 10*5 = 546 microdollars.
	accountingCost(t, row.Cost, .000546)
	if row.Source != "estimated" {
		t.Fatalf("source: %q", row.Source)
	}
}

func TestAccountingRetentionKeepsNewestInsertedAfterRestart(t *testing.T) {
	provider := accountingProvider(t)
	config := accountingConfig(provider.URL, filepath.Join(t.TempDir(), "usage.db"), 2)
	p := startRouter(t, config)
	for _, alias := range []string{"measured", "zero", "missing"} {
		accountingInfer(t, p, alias)
	}
	p.Stop(t, syscall.SIGTERM)
	p = startRouter(t, config)
	report := accountingGet(t, p, "")
	if report.Totals.Requests != 2 || len(report.Requests) != 2 {
		t.Fatalf("retained count: %+v", report)
	}
	aliases := map[string]bool{}
	for _, row := range report.Requests {
		aliases[row.Alias] = true
	}
	if !aliases["zero"] || !aliases["missing"] || aliases["measured"] {
		t.Fatalf("not newest inserted: %+v", aliases)
	}
	accountingInfer(t, p, "anthropic")
	p.Stop(t, syscall.SIGTERM)
	p = startRouter(t, config)
	report = accountingGet(t, p, "")
	aliases = map[string]bool{}
	for _, row := range report.Requests {
		aliases[row.Alias] = true
	}
	if report.Totals.Requests != 2 || !aliases["missing"] || !aliases["anthropic"] || aliases["zero"] {
		t.Fatalf("retention across reopen: %+v", report)
	}
}

func TestAccountingDisabledHistoryStillServesInference(t *testing.T) {
	var hits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()
	p := startRouter(t, accountingConfig(provider.URL, "", 0))
	accountingInfer(t, p, "measured")
	result := request(t, "GET", p.URL+"/usage", "", map[string]string{"Authorization": "Bearer " + gatewayKey})
	if result.Status != 404 {
		t.Fatalf("disabled usage: %d %s", result.Status, result.Body)
	}
	body := decodeJSON(t, result.Body)
	failure, ok := body["error"].(map[string]any)
	if !ok || failure["type"] != "usage_store_disabled" {
		t.Fatalf("disabled error: %s", result.Body)
	}
	if hits.Load() != 1 {
		t.Fatalf("served dispatches = %d", hits.Load())
	}
}
