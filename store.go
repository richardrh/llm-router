package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

var errUsageStoreDisabled = errors.New("no usage store is configured")

// The usage store is a local record of what the router served, in a file any
// SQLite client can read. It is deliberately not a control plane: no keys, no
// budgets, no tenancy. It answers "what did this router do, and what did it
// cost" without requiring a database server, and the file is the whole state.

const (
	// defaultUsageQueue bounds how many records may wait for the writer. A full
	// queue drops records rather than blocking a request: accounting must never
	// become the reason an inference call is slow or fails.
	defaultUsageQueue = 1024
	usageBatchSize    = 64
	usageFlushEvery   = 250 * time.Millisecond

	// sqlTimeFormat is fixed-width so that lexicographic comparison in SQLite
	// matches chronological order. time.RFC3339Nano trims trailing zeros, which
	// makes "…:00Z" sort after "…:00.5Z" and silently breaks range queries.
	sqlTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"
)

// UsageRecord is one served request, and does double duty as the persisted row
// and as the JSON shape /usage returns.
//
// The token counts are pointers because "the provider reported nothing" and
// "the provider reported zero" are different facts, and only one of them
// licenses a cost. Storing NULL keeps that distinction in the database rather
// than in a convention every future query would have to remember: SUM and AVG
// skip NULL, so a spend total is never inflated by a request whose usage was
// never measured.
type UsageRecord struct {
	Timestamp        time.Time `json:"ts"`
	RequestID        string    `json:"request_id"`
	Alias            string    `json:"alias"`
	Upstream         string    `json:"upstream"`
	UpstreamModel    string    `json:"upstream_model"`
	Status           int       `json:"status"`
	Attempts         int       `json:"attempts"`
	Streamed         bool      `json:"streamed"`
	Bytes            int64     `json:"bytes"`
	TotalMS          int64     `json:"total_ms"`
	InputTokens      *int      `json:"input_tokens"`
	OutputTokens     *int      `json:"output_tokens"`
	CacheReadTokens  *int      `json:"cache_read_tokens"`
	CacheWriteTokens *int      `json:"cache_write_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	// CostSource is "reported" (the upstream stated its own spend, as a
	// CLI-backed agent does), "estimated" (from the alias's declared rates) or
	// "unreported" (nothing was measured, so no cost is claimed).
	CostSource string `json:"cost_source"`
}

const usageSchema = `
CREATE TABLE IF NOT EXISTS requests (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                 TEXT    NOT NULL,
    request_id         TEXT    NOT NULL,
    alias              TEXT    NOT NULL,
    upstream           TEXT    NOT NULL,
    upstream_model     TEXT    NOT NULL,
    status             INTEGER NOT NULL,
    attempts           INTEGER NOT NULL,
    streamed           INTEGER NOT NULL,
    bytes              INTEGER NOT NULL,
    total_ms           INTEGER NOT NULL,
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    cache_read_tokens  INTEGER,
    cache_write_tokens INTEGER,
    cost_usd           REAL    NOT NULL,
    cost_source        TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_requests_ts    ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_alias ON requests(alias, ts);
`

const usageColumnList = `ts, request_id, alias, upstream, upstream_model, status, attempts,
	streamed, bytes, total_ms, input_tokens, output_tokens, cache_read_tokens,
	cache_write_tokens, cost_usd, cost_source`

const usageInsert = `INSERT INTO requests (` + usageColumnList + `)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// Store owns the usage database and the goroutine that writes to it.
type Store struct {
	db   *sql.DB
	ch   chan UsageRecord
	done chan struct{}
	// Close is idempotent: shutdown paths and tests both close the store, and a
	// second close of a channel would panic.
	closeOnce sync.Once
	closeErr  error
	log       *slog.Logger
	dropped   atomic.Uint64
}

// openUsageStore opens the configured usage database, or returns a nil store
// when persistence is switched off. A nil store records nothing, which is how
// the rest of the code stays free of an "is it enabled" branch.
func openUsageStore(cfg *Config, log *slog.Logger) (*Store, error) {
	if cfg.Store.Path == "" {
		return nil, nil
	}
	return OpenStore(cfg.Store.Path, cfg.Store.QueueSize, log)
}

// expandHome resolves a leading ~ so a config can name ~/.omp-router/usage.db
// without the router creating a literal directory called "~".
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve ~ in %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// OpenStore opens or creates the usage database at path and starts the writer.
// A zero queueSize takes the default.
func OpenStore(path string, queueSize int, log *slog.Logger) (*Store, error) {
	path, err := expandHome(path)
	if err != nil {
		return nil, fmt.Errorf("usage store: %w", err)
	}
	if queueSize <= 0 {
		queueSize = defaultUsageQueue
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("usage store: create %s: %w", dir, err)
		}
	}

	// A URI DSN is what carries the per-connection pragmas; they are applied to
	// every connection the pool opens, which a one-off PRAGMA after Open would
	// not be. WAL matters twice over: it keeps the writer from blocking an
	// external reader with the database open in the sqlite3 CLI, and it stops a
	// slow aggregate from stalling the write path.
	dsn := url.URL{Scheme: "file", Path: path}
	q := dsn.Query()
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("usage store: open %s: %w", path, err)
	}
	// Readers run concurrently with the writer under WAL; a single connection
	// would serialise /usage behind a flush.
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(usageSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("usage store: initialise %s: %w", path, err)
	}

	s := &Store{
		db:   db,
		ch:   make(chan UsageRecord, queueSize),
		done: make(chan struct{}),
		log:  log,
	}
	go s.run()
	return s, nil
}

// Record queues one served request. It never blocks and never fails, so it can
// sit directly on the request path. A nil store is a no-op, which is how the
// feature stays off without every caller testing for it.
func (s *Store) Record(r UsageRecord) {
	if s == nil {
		return
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now()
	}
	select {
	case s.ch <- r:
	default:
		// The disk cannot keep up. Dropping is the right failure: the
		// alternative is adding latency to inference calls, or failing them,
		// because a usage log is behind. Dropped() surfaces the loss rather
		// than letting the totals quietly under-report.
		s.dropped.Add(1)
	}
}

// Dropped is the number of records discarded because the queue was full.
func (s *Store) Dropped() uint64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

// run drains the queue into the database, batching so that a burst of requests
// costs one transaction rather than one per request.
func (s *Store) run() {
	defer close(s.done)
	tick := time.NewTicker(usageFlushEvery)
	defer tick.Stop()

	batch := make([]UsageRecord, 0, usageBatchSize)
	for {
		select {
		case rec, ok := <-s.ch:
			if !ok {
				s.flush(batch)
				return
			}
			batch = append(batch, rec)
			if len(batch) >= usageBatchSize {
				s.flush(batch)
				batch = batch[:0]
			}
		case <-tick.C:
			if len(batch) > 0 {
				s.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

func (s *Store) flush(batch []UsageRecord) {
	if len(batch) == 0 {
		return
	}
	if err := s.insert(batch); err != nil {
		// Logged, not returned: there is no caller left to fail. The rows are
		// lost, which is the honest outcome when the database is unwritable.
		s.log.Error("usage store: write failed", "rows", len(batch), "error", err)
	}
}

func (s *Store) insert(batch []UsageRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(usageInsert)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range batch {
		if _, err := stmt.Exec(
			r.Timestamp.UTC().Format(sqlTimeFormat),
			r.RequestID, r.Alias, r.Upstream, r.UpstreamModel,
			r.Status, r.Attempts, r.Streamed, r.Bytes, r.TotalMS,
			r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens,
			r.CostUSD, r.CostSource,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close flushes what is queued and releases the database. It is safe on a nil
// store, so shutdown needs no special case for the feature being off, and safe
// to call twice.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		close(s.ch)
		<-s.done
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// UsageFilter narrows a usage query. Zero fields are not applied, so the zero
// filter means "everything recorded".
type UsageFilter struct {
	Since    time.Time
	Until    time.Time
	Alias    string
	Upstream string
	Limit    int
}

// UsageTotals is the aggregate over the rows a filter selects.
type UsageTotals struct {
	Requests         int64   `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	// Unreported counts requests whose provider stated no usage. Their tokens
	// are absent from the sums above rather than contributing zeros, so a total
	// always describes only what was actually measured.
	Unreported int64 `json:"unreported"`
}

// UsageGroup is one row of a grouped report.
type UsageGroup struct {
	Key string `json:"key"`
	UsageTotals
}

// usageGroupColumns is the whitelist of groupable columns. It exists because a
// column name cannot be a bound parameter, so it has to be interpolated, and
// interpolating arbitrary query input would be an injection.
var usageGroupColumns = map[string]string{
	"alias":          "alias",
	"upstream":       "upstream",
	"upstream_model": "upstream_model",
	"cost_source":    "cost_source",
}

// UsageGroupColumns lists the accepted group_by values.
func UsageGroupColumns() []string {
	return []string{"alias", "upstream", "upstream_model", "cost_source"}
}

func (f UsageFilter) where() (string, []any) {
	var conds []string
	var args []any
	if !f.Since.IsZero() {
		conds = append(conds, "ts >= ?")
		args = append(args, f.Since.UTC().Format(sqlTimeFormat))
	}
	if !f.Until.IsZero() {
		conds = append(conds, "ts < ?")
		args = append(args, f.Until.UTC().Format(sqlTimeFormat))
	}
	if f.Alias != "" {
		conds = append(conds, "alias = ?")
		args = append(args, f.Alias)
	}
	if f.Upstream != "" {
		conds = append(conds, "upstream = ?")
		args = append(args, f.Upstream)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

const usageAggregateList = `COUNT(*),
	COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(cache_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0),
	COALESCE(SUM(cost_usd), 0),
	COALESCE(SUM(CASE WHEN cost_source = 'unreported' THEN 1 ELSE 0 END), 0)`

func scanTotals(row interface{ Scan(...any) error }) (UsageTotals, error) {
	var t UsageTotals
	err := row.Scan(&t.Requests, &t.InputTokens, &t.OutputTokens,
		&t.CacheReadTokens, &t.CacheWriteTokens, &t.CostUSD, &t.Unreported)
	return t, err
}

// Totals reports the aggregate over the filtered window.
func (s *Store) Totals(f UsageFilter) (UsageTotals, error) {
	if s == nil {
		return UsageTotals{}, errUsageStoreDisabled
	}
	where, args := f.where()
	return scanTotals(s.db.QueryRow(`SELECT `+usageAggregateList+` FROM requests`+where, args...))
}

// Groups reports the aggregate broken down by one whitelisted column.
func (s *Store) Groups(f UsageFilter, column string) ([]UsageGroup, error) {
	if s == nil {
		return nil, errUsageStoreDisabled
	}
	col, ok := usageGroupColumns[column]
	if !ok {
		return nil, fmt.Errorf("cannot group by %q", column)
	}
	where, args := f.where()
	rows, err := s.db.Query(
		`SELECT `+col+`, `+usageAggregateList+` FROM requests`+where+
			` GROUP BY `+col+` ORDER BY SUM(cost_usd) DESC, COUNT(*) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UsageGroup
	for rows.Next() {
		var g UsageGroup
		if err := rows.Scan(&g.Key, &g.Requests, &g.InputTokens, &g.OutputTokens,
			&g.CacheReadTokens, &g.CacheWriteTokens, &g.CostUSD, &g.Unreported); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Recent returns the most recent rows, newest first.
func (s *Store) Recent(f UsageFilter) ([]UsageRecord, error) {
	if s == nil {
		return nil, errUsageStoreDisabled
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	where, args := f.where()
	args = append(args, limit)

	rows, err := s.db.Query(
		`SELECT `+usageColumnList+` FROM requests`+where+` ORDER BY ts DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]UsageRecord, 0, limit)
	for rows.Next() {
		var (
			r          UsageRecord
			ts         string
			streamed   int
			in, outT   sql.NullInt64
			cacheR, cW sql.NullInt64
		)
		if err := rows.Scan(&ts, &r.RequestID, &r.Alias, &r.Upstream, &r.UpstreamModel,
			&r.Status, &r.Attempts, &streamed, &r.Bytes, &r.TotalMS,
			&in, &outT, &cacheR, &cW, &r.CostUSD, &r.CostSource); err != nil {
			return nil, err
		}
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			r.Timestamp = parsed
		}
		r.Streamed = streamed != 0
		r.InputTokens = nullInt(in)
		r.OutputTokens = nullInt(outT)
		r.CacheReadTokens = nullInt(cacheR)
		r.CacheWriteTokens = nullInt(cW)
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

// parseUsageTime accepts an absolute RFC3339 timestamp or a relative duration
// such as "24h", "7d" or "90m", which is what an operator actually types.
func parseUsageTime(raw string, now time.Time) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if d, err := parseUsageDuration(raw); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("expected an RFC3339 timestamp or a duration like 24h, got %q", raw)
}

// parseUsageDuration extends Go's duration syntax with days, because a usage
// window is naturally expressed in them and "168h" is needless arithmetic.
func parseUsageDuration(raw string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(raw, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(raw)
}

// usageResponse is the body of a /usage report.
type usageResponse struct {
	Since   string       `json:"since,omitempty"`
	Until   string       `json:"until,omitempty"`
	GroupBy string       `json:"group_by,omitempty"`
	Totals  UsageTotals  `json:"totals"`
	Groups  []UsageGroup `json:"groups,omitempty"`
	// Requests carries individual rows when no grouping was asked for.
	Requests []UsageRecord `json:"requests,omitempty"`
	// DroppedRecords is how many records the store discarded because the writer
	// could not keep up. It is reported so an under-count is visible rather than
	// quietly wrong.
	DroppedRecords uint64 `json:"dropped_records"`
}

// handleUsage serves the recorded usage. This is the router's own reporting
// surface, not part of the OpenRouter-compatible API, so it is named for what
// it is rather than given a /v1 path it does not honour.
//
//	GET /usage                       recent requests
//	GET /usage?group_by=alias        totals per alias
//	GET /usage?since=7d&alias=…      a window, filtered
//
// Accepted parameters: since and until (RFC3339 or a duration like 24h, 7d),
// alias, upstream, limit, group_by (alias, upstream, upstream_model,
// cost_source).
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}
	// Spend data reveals which providers serve you and what they cost, so it
	// carries the same credential as inference rather than being public like
	// /healthz.
	if !s.authorized(w, r) {
		return
	}
	if s.store == nil {
		s.writeError(w, http.StatusNotFound, "usage_store_disabled",
			"no usage store is configured; set store.path in router.yaml")
		return
	}

	q := r.URL.Query()
	now := time.Now()

	since, err := parseUsageTime(q.Get("since"), now)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "since: "+err.Error())
		return
	}
	until, err := parseUsageTime(q.Get("until"), now)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "until: "+err.Error())
		return
	}

	filter := UsageFilter{
		Since:    since,
		Until:    until,
		Alias:    q.Get("alias"),
		Upstream: q.Get("upstream"),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			s.writeError(w, http.StatusBadRequest, "bad_request",
				"limit must be a positive integer")
			return
		}
		filter.Limit = n
	}

	resp := usageResponse{
		GroupBy:        q.Get("group_by"),
		DroppedRecords: s.store.Dropped(),
	}
	if !since.IsZero() {
		resp.Since = since.UTC().Format(time.RFC3339)
	}
	if !until.IsZero() {
		resp.Until = until.UTC().Format(time.RFC3339)
	}

	if resp.GroupBy != "" {
		groups, err := s.store.Groups(filter, resp.GroupBy)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		resp.Groups = groups
	} else {
		rows, err := s.store.Recent(filter)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "usage_query_failed", err.Error())
			return
		}
		resp.Requests = rows
	}

	totals, err := s.store.Totals(filter)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "usage_query_failed", err.Error())
		return
	}
	resp.Totals = totals

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
