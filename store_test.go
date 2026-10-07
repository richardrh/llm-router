package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discardStoreLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedUsage writes records, closes the store so they are flushed to disk, and
// reopens it. The writer is asynchronous by design, so a test that queried
// immediately would be racing it rather than testing it.
func seedUsage(t *testing.T, path string, recs ...UsageRecord) *Store {
	t.Helper()
	return seedUsageOpts(t, StoreOptions{Path: path}, recs...)
}

// seedUsageOpts is seedUsage with control over the store's options, so the row
// cap can be exercised. It writes, closes, and reopens: the cap is enforced at
// open, which is exactly the restart path worth testing.
func seedUsageOpts(t *testing.T, opts StoreOptions, recs ...UsageRecord) *Store {
	t.Helper()
	s, err := OpenStore(opts, discardStoreLog())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	for _, r := range recs {
		s.Record(r)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenStore(opts, discardStoreLog())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

// usageSequence builds count records with strictly increasing ids and
// timestamps, starting at from, so tests can name exactly which records should
// survive a prune.
func usageSequence(base time.Time, from, count int) []UsageRecord {
	recs := make([]UsageRecord, 0, count)
	for i := range count {
		recs = append(recs, UsageRecord{
			Timestamp:     base.Add(time.Duration(i) * time.Second),
			RequestID:     fmt.Sprintf("r%03d", from+i),
			Alias:         "a",
			Upstream:      "u",
			UpstreamModel: "m",
			CostSource:    "unreported",
		})
	}
	return recs
}

// TestUsageStoreUsesWAL checks the DSN pragmas actually reached the driver.
// WAL is what lets an external reader query the file while the router writes,
// and it is applied through a URL-escaped connection string, so it is worth
// asserting rather than assuming.
func TestUsageStoreUsesWAL(t *testing.T) {
	s := seedUsage(t, filepath.Join(t.TempDir(), "usage.db"))

	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}

	var busy int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busy == 0 {
		t.Fatalf("busy_timeout = 0, want the configured 5000")
	}

	// The write-ahead log is part of the footprint on disk. Left at the default
	// threshold it outgrew the database by three orders of magnitude, so the
	// bound is asserted rather than assumed.
	var checkpoint int
	if err := s.db.QueryRow("PRAGMA wal_autocheckpoint").Scan(&checkpoint); err != nil {
		t.Fatalf("PRAGMA wal_autocheckpoint: %v", err)
	}
	if checkpoint != 64 {
		t.Errorf("wal_autocheckpoint = %d, want 64", checkpoint)
	}
}

// TestUsageStorePersistsAndKeepsUnreportedNull is the honesty requirement: a
// request whose provider reported nothing must store NULL, not zero, so that
// SUM over the column cannot invent spend that was never measured.
func TestUsageStorePersistsAndKeepsUnreportedNull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.db")
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	s := seedUsage(t, path,
		UsageRecord{
			Timestamp: base, RequestID: "r1", Alias: "a/model", Upstream: "openrouter",
			UpstreamModel: "m", Status: 200, Attempts: 1, Streamed: true, Bytes: 10,
			TotalMS: 5, InputTokens: new(100), OutputTokens: new(20),
			CacheReadTokens: new(7), CacheWriteTokens: new(3),
			CostUSD: 0.5, CostSource: "estimated",
		},
		UsageRecord{
			Timestamp: base.Add(time.Second), RequestID: "r2", Alias: "a/model",
			Upstream: "anthropic", UpstreamModel: "m2", Status: 200, Attempts: 2,
			TotalMS: 9, CostSource: "unreported",
		},
	)

	rows, err := s.Recent(UsageFilter{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Newest first.
	if rows[0].RequestID != "r2" {
		t.Errorf("rows[0] = %q, want r2 (newest first)", rows[0].RequestID)
	}
	if rows[0].InputTokens != nil || rows[0].OutputTokens != nil {
		t.Errorf("unreported usage must round-trip as NULL, got %v/%v",
			rows[0].InputTokens, rows[0].OutputTokens)
	}
	if rows[0].CostSource != "unreported" {
		t.Errorf("CostSource = %q, want unreported", rows[0].CostSource)
	}
	reported := rows[1]
	if reported.InputTokens == nil || *reported.InputTokens != 100 {
		t.Errorf("InputTokens = %v, want 100", reported.InputTokens)
	}
	if reported.CacheReadTokens == nil || *reported.CacheReadTokens != 7 {
		t.Errorf("CacheReadTokens = %v, want 7", reported.CacheReadTokens)
	}
	if !reported.Streamed {
		t.Errorf("Streamed = false, want true")
	}
	if !reported.Timestamp.Equal(base) {
		t.Errorf("Timestamp = %v, want %v", reported.Timestamp, base)
	}

	totals, err := s.Totals(UsageFilter{})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if totals.Requests != 2 {
		t.Errorf("Requests = %d, want 2", totals.Requests)
	}
	// The unreported row contributes NULLs, so the sums describe only what was
	// actually measured.
	if totals.InputTokens != 100 || totals.OutputTokens != 20 {
		t.Errorf("tokens = %d/%d, want 100/20 (unreported must not add zeros)",
			totals.InputTokens, totals.OutputTokens)
	}
	if totals.CostUSD != 0.5 {
		t.Errorf("CostUSD = %v, want 0.5", totals.CostUSD)
	}
	if totals.Unreported != 1 {
		t.Errorf("Unreported = %d, want 1", totals.Unreported)
	}
}

// TestUsageStoreOrdersAndFiltersSubsecondTimestamps guards the timestamp
// format. time.RFC3339Nano trims trailing zeros, which makes "…:00Z" sort
// after "…:00.5Z" as text and silently inverts both ordering and range
// filters. The format is fixed-width precisely to avoid that.
func TestUsageStoreOrdersAndFiltersSubsecondTimestamps(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := seedUsage(t, filepath.Join(t.TempDir(), "usage.db"),
		UsageRecord{Timestamp: base, RequestID: "whole-second", Alias: "a", Upstream: "u", UpstreamModel: "m", CostSource: "unreported"},
		UsageRecord{Timestamp: base.Add(500 * time.Millisecond), RequestID: "half-second", Alias: "a", Upstream: "u", UpstreamModel: "m", CostSource: "unreported"},
	)

	rows, err := s.Recent(UsageFilter{})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].RequestID != "half-second" {
		t.Fatalf("newest row = %q, want half-second; sub-second timestamps are sorting as text",
			rows[0].RequestID)
	}

	// A window opening between the two must select only the later one.
	cut := base.Add(250 * time.Millisecond)
	rows, err = s.Recent(UsageFilter{Since: cut})
	if err != nil {
		t.Fatalf("Recent(since): %v", err)
	}
	if len(rows) != 1 || rows[0].RequestID != "half-second" {
		t.Fatalf("since-filter returned %d rows (%v), want just half-second", len(rows), rows)
	}

	totals, err := s.Totals(UsageFilter{Until: cut})
	if err != nil {
		t.Fatalf("Totals(until): %v", err)
	}
	if totals.Requests != 1 {
		t.Errorf("Until-filter counted %d requests, want 1", totals.Requests)
	}
}

// TestUsageStoreGroups covers the aggregate breakdown /usage exposes.
func TestUsageStoreGroups(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := seedUsage(t, filepath.Join(t.TempDir(), "usage.db"),
		UsageRecord{Timestamp: base, RequestID: "1", Alias: "claude", Upstream: "openrouter", UpstreamModel: "m", CostUSD: 0.25, CostSource: "estimated"},
		UsageRecord{Timestamp: base, RequestID: "2", Alias: "claude", Upstream: "openrouter", UpstreamModel: "m", CostUSD: 0.25, CostSource: "estimated"},
		UsageRecord{Timestamp: base, RequestID: "3", Alias: "gpt", Upstream: "openai", UpstreamModel: "m", CostUSD: 0.10, CostSource: "estimated"},
	)

	groups, err := s.Groups(UsageFilter{}, "alias")
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	// Ordered by cost, so the two-request alias leads.
	if groups[0].Key != "claude" || groups[0].Requests != 2 || groups[0].CostUSD != 0.5 {
		t.Errorf("groups[0] = %+v, want claude/2/0.5", groups[0])
	}
	if groups[1].Key != "gpt" || groups[1].CostUSD != 0.10 {
		t.Errorf("groups[1] = %+v, want gpt/0.10", groups[1])
	}

	// Filtering narrows the aggregate.
	one, err := s.Groups(UsageFilter{Alias: "gpt"}, "alias")
	if err != nil {
		t.Fatalf("Groups(alias=gpt): %v", err)
	}
	if len(one) != 1 || one[0].Requests != 1 {
		t.Fatalf("filtered groups = %+v, want one gpt row", one)
	}

	// A column name cannot be bound, so it is whitelisted; anything else is
	// refused rather than interpolated.
	if _, err := s.Groups(UsageFilter{}, "1; DROP TABLE requests"); err == nil {
		t.Fatal("Groups accepted a non-whitelisted column")
	}
}

// TestUsageStoreDropsRatherThanBlocks pins the guarantee that accounting can
// never slow a request down or fail one. The store is built by hand with no
// writer running, so the queue fills deterministically.
func TestUsageStoreDropsRatherThanBlocks(t *testing.T) {
	s := &Store{
		ch:   make(chan UsageRecord, 1),
		done: make(chan struct{}),
		log:  discardStoreLog(),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5 {
			s.Record(UsageRecord{RequestID: "r"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on a full queue; it must always return")
	}
	if got := s.Dropped(); got != 4 {
		t.Fatalf("Dropped = %d, want 4 (one fit, four discarded)", got)
	}
}

// TestNilUsageStoreIsInert covers the disabled configuration, which is
// represented by a nil store rather than a flag.
func TestNilUsageStoreIsInert(t *testing.T) {
	var s *Store
	s.Record(UsageRecord{RequestID: "ignored"}) // must not panic
	if s.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0", s.Dropped())
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if _, err := s.Totals(UsageFilter{}); !errors.Is(err, errUsageStoreDisabled) {
		t.Errorf("Totals = %v, want errUsageStoreDisabled", err)
	}
	if _, err := s.Recent(UsageFilter{}); !errors.Is(err, errUsageStoreDisabled) {
		t.Errorf("Recent = %v, want errUsageStoreDisabled", err)
	}
}

func TestUsageStoreCloseIsIdempotent(t *testing.T) {
	s, err := OpenStore(StoreOptions{Path: filepath.Join(t.TempDir(), "usage.db")}, discardStoreLog())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// waitForPruned blocks until the writer has processed every queued record and
// enforced the cap. Flushes happen on a 250ms tick, so a prune test has to wait
// rather than assume. It waits on the pruned total rather than the row count,
// because the row count reaches the cap on the first flush while records are
// still queued.
func waitForPruned(t *testing.T, s *Store, want uint64) StoreStats {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stats, err := s.Stats()
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if stats.Pruned >= want {
			return stats
		}
		if time.Now().After(deadline) {
			t.Fatalf("Pruned = %d, want %d", stats.Pruned, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestUsageStorePrunesToTheRowBudget is the retention contract: the cap is
// enforced while running, and it is the oldest records that go.
func TestUsageStorePrunesToTheRowBudget(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s, err := OpenStore(StoreOptions{
		Path:    filepath.Join(t.TempDir(), "usage.db"),
		MaxRows: 50,
	}, discardStoreLog())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, r := range usageSequence(base, 0, 200) {
		s.Record(r)
	}

	stats := waitForPruned(t, s, 150)
	if stats.Rows != 50 {
		t.Errorf("Rows = %d, want 50", stats.Rows)
	}
	if stats.MaxRows != 50 {
		t.Errorf("MaxRows = %d, want 50", stats.MaxRows)
	}
	if stats.Pruned != 150 {
		t.Errorf("Pruned = %d, want 150", stats.Pruned)
	}
	if stats.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %d, want the file's size", stats.SizeBytes)
	}

	rows, err := s.Recent(UsageFilter{Limit: 1000})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(rows) != 50 {
		t.Fatalf("got %d rows, want 50", len(rows))
	}
	// The newest must survive and the oldest must be r150. A cap that deleted
	// arbitrary rows would satisfy the count but destroy recent history.
	if rows[0].RequestID != "r199" {
		t.Errorf("newest = %q, want r199", rows[0].RequestID)
	}
	if last := rows[len(rows)-1].RequestID; last != "r150" {
		t.Errorf("oldest surviving = %q, want r150", last)
	}
}

// TestUsageStoreRowBudgetIsARing checks the cap across restarts: it is a ring of
// the newest N records, not a one-off trim.
func TestUsageStoreRowBudgetIsARing(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	opts := StoreOptions{Path: filepath.Join(t.TempDir(), "usage.db"), MaxRows: 20}

	s := seedUsageOpts(t, opts, usageSequence(base, 0, 50)...)
	// r030..r049 survived the first prune; five more arrive.
	for _, r := range usageSequence(base.Add(time.Hour), 50, 5) {
		s.Record(r)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenStore(opts, discardStoreLog())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	stats, err := reopened.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Rows != 20 {
		t.Fatalf("Rows = %d, want 20 after further arrivals", stats.Rows)
	}
	rows, err := reopened.Recent(UsageFilter{Limit: 100})
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if rows[0].RequestID != "r054" {
		t.Errorf("newest = %q, want r054", rows[0].RequestID)
	}
	if last := rows[len(rows)-1].RequestID; last != "r035" {
		t.Errorf("oldest surviving = %q, want r035", last)
	}
}

// TestUsageStoreWithoutACapKeepsEverything pins the default. No budget means
// nothing is deleted: discarding a user's history unasked would be worse than an
// unbounded file they can prune themselves.
func TestUsageStoreWithoutACapKeepsEverything(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := seedUsage(t, filepath.Join(t.TempDir(), "usage.db"), usageSequence(base, 0, 300)...)

	stats, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Rows != 300 {
		t.Errorf("Rows = %d, want 300", stats.Rows)
	}
	if stats.Pruned != 0 {
		t.Errorf("Pruned = %d, want 0", stats.Pruned)
	}
	if stats.MaxRows != 0 {
		t.Errorf("MaxRows = %d, want 0", stats.MaxRows)
	}
}

func TestParseUsageTime(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"", time.Time{}},
		{"24h", now.Add(-24 * time.Hour)},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"90m", now.Add(-90 * time.Minute)},
		{"2026-10-01T00:00:00Z", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := parseUsageTime(tc.in, now)
		if err != nil {
			t.Errorf("parseUsageTime(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("parseUsageTime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"yesterday", "3", "1d2h30"} {
		if _, err := parseUsageTime(bad, now); err == nil {
			t.Errorf("parseUsageTime(%q) accepted an unparseable window", bad)
		}
	}
}

// usageServer builds the smallest server the reporting handler needs.
func usageServer(apiKey string, store *Store) *Server {
	return &Server{cfg: &Config{APIKey: apiKey}, log: discardStoreLog(), store: store}
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
		UsageRecord{Timestamp: base, RequestID: "r1", Alias: "claude", Upstream: "openrouter",
			UpstreamModel: "anthropic/x", Status: 200, Attempts: 1, Streamed: true,
			InputTokens: new(1000), OutputTokens: new(200),
			CostUSD: 0.02, CostSource: "estimated"},
		UsageRecord{Timestamp: base.Add(time.Second), RequestID: "r2", Alias: "gpt",
			Upstream: "openai", UpstreamModel: "gpt/y", Status: 200, Attempts: 1,
			CostSource: "unreported"},
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
	if len(body.Requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(body.Requests))
	}
	if body.Requests[0].RequestID != "r2" {
		t.Errorf("newest request = %q, want r2", body.Requests[0].RequestID)
	}
	if body.Totals.Requests != 2 || body.Totals.InputTokens != 1000 || body.Totals.CostUSD != 0.02 {
		t.Errorf("totals = %+v", body.Totals)
	}
	if body.Totals.Unreported != 1 {
		t.Errorf("totals.Unreported = %d, want 1", body.Totals.Unreported)
	}
	if body.Store.Rows != 2 {
		t.Errorf("store.Rows = %d, want 2", body.Store.Rows)
	}
	if body.Store.SizeBytes <= 0 {
		t.Errorf("store.SizeBytes = %d, want the file's size", body.Store.SizeBytes)
	}

	// Grouped view.
	res = get(t, srv, "/usage?group_by=alias", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("grouped /usage = %d, want 200", res.StatusCode)
	}
	grouped := decodeUsage(t, res)
	if grouped.GroupBy != "alias" {
		t.Errorf("group_by = %q, want alias", grouped.GroupBy)
	}
	if len(grouped.Groups) != 2 || grouped.Groups[0].Key != "claude" {
		t.Fatalf("groups = %+v", grouped.Groups)
	}
	if len(grouped.Requests) != 0 {
		t.Errorf("a grouped report should not also carry raw rows, got %d", len(grouped.Requests))
	}

	// Aliasing and limiting.
	res = get(t, srv, "/usage?alias=claude&limit=1", nil)
	filtered := decodeUsage(t, res)
	if len(filtered.Requests) != 1 || filtered.Requests[0].Alias != "claude" {
		t.Errorf("alias filter returned %+v", filtered.Requests)
	}
	if filtered.Totals.Requests != 1 {
		t.Errorf("filtered totals.Requests = %d, want 1", filtered.Totals.Requests)
	}
}

func TestUsageEndpointGuards(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := seedUsage(t, filepath.Join(t.TempDir(), "usage.db"),
		UsageRecord{Timestamp: base, RequestID: "r1", Alias: "a", Upstream: "u",
			UpstreamModel: "m", CostSource: "unreported"},
	)

	t.Run("spend data requires the gateway key", func(t *testing.T) {
		srv := usageServer("secret", store)
		if res := get(t, srv, "/usage", nil); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated /usage = %d, want 401", res.StatusCode)
		}
		res := get(t, srv, "/usage", map[string]string{"Authorization": "Bearer secret"})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("authenticated /usage = %d, want 200", res.StatusCode)
		}
	})

	t.Run("disabled store is reported, not silently empty", func(t *testing.T) {
		srv := usageServer("", nil)
		res := get(t, srv, "/usage", nil)
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("GET /usage with no store = %d, want 404", res.StatusCode)
		}
	})

	t.Run("bad parameters are rejected", func(t *testing.T) {
		srv := usageServer("", store)
		for _, path := range []string{
			"/usage?group_by=password",
			"/usage?since=yesterday",
			"/usage?until=not-a-time",
			"/usage?limit=0",
			"/usage?limit=abc",
		} {
			if res := get(t, srv, path, nil); res.StatusCode != http.StatusBadRequest {
				t.Errorf("GET %s = %d, want 400", path, res.StatusCode)
			}
		}
	})

	t.Run("non-GET is refused", func(t *testing.T) {
		srv := usageServer("", store)
		if res := post(t, srv, "/usage", map[string]any{}, nil); res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST /usage = %d, want 405", res.StatusCode)
		}
	})
}
