package router

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/richardrh/llm-router/internal/usage"
)

type usageResponse struct {
	Since          string              `json:"since,omitempty"`
	Until          string              `json:"until,omitempty"`
	GroupBy        string              `json:"group_by,omitempty"`
	Totals         usage.UsageTotals   `json:"totals"`
	Groups         []usage.UsageGroup  `json:"groups,omitempty"`
	Requests       []usage.UsageRecord `json:"requests,omitempty"`
	DroppedRecords uint64              `json:"dropped_records"`
	Store          usage.StoreStats    `json:"store"`
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
		return
	}
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
	since, err := usage.ParseUsageTime(q.Get("since"), now)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "since: "+err.Error())
		return
	}
	until, err := usage.ParseUsageTime(q.Get("until"), now)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "until: "+err.Error())
		return
	}

	filter := usage.UsageFilter{
		Since: since, Until: until,
		Alias: q.Get("alias"), Upstream: q.Get("upstream"),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			s.writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		filter.Limit = n
	}

	resp := usageResponse{GroupBy: q.Get("group_by"), DroppedRecords: s.store.Dropped()}
	if !since.IsZero() {
		resp.Since = since.UTC().Format(time.RFC3339)
	}
	if !until.IsZero() {
		resp.Until = until.UTC().Format(time.RFC3339)
	}

	if resp.GroupBy != "" {
		resp.Groups, err = s.store.Groups(filter, resp.GroupBy)
		if err != nil {
			s.writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	} else {
		resp.Requests, err = s.store.Recent(filter)
		if err != nil {
			s.writeError(w, http.StatusInternalServerError, "usage_query_failed", err.Error())
			return
		}
	}
	resp.Totals, err = s.store.Totals(filter)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "usage_query_failed", err.Error())
		return
	}
	resp.Store, err = s.store.Stats()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "usage_query_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
