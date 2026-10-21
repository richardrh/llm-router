package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"llm-router/internal/usage"
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

	resp, status, err := s.queryUsage(r.URL.Query())
	if err != nil {
		code := "bad_request"
		if status == http.StatusInternalServerError {
			code = "usage_query_failed"
		}
		s.writeError(w, status, code, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// queryUsage is shared by JSON reporting and the server-rendered console.
func (s *Server) queryUsage(q url.Values) (usageResponse, int, error) {
	now := time.Now()
	since, err := usage.ParseUsageTime(q.Get("since"), now)
	if err != nil {
		return usageResponse{}, http.StatusBadRequest, fmt.Errorf("since: %w", err)
	}
	until, err := usage.ParseUsageTime(q.Get("until"), now)
	if err != nil {
		return usageResponse{}, http.StatusBadRequest, fmt.Errorf("until: %w", err)
	}

	filter := usage.UsageFilter{
		Since: since, Until: until,
		Alias: q.Get("alias"), Upstream: q.Get("upstream"),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return usageResponse{}, http.StatusBadRequest, fmt.Errorf("limit must be a positive integer")
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
			return usageResponse{}, http.StatusBadRequest, err
		}
	} else {
		resp.Requests, err = s.store.Recent(filter)
		if err != nil {
			return usageResponse{}, http.StatusInternalServerError, err
		}
	}
	resp.Totals, err = s.store.Totals(filter)
	if err != nil {
		return usageResponse{}, http.StatusInternalServerError, err
	}
	resp.Store, err = s.store.Stats()
	if err != nil {
		return usageResponse{}, http.StatusInternalServerError, err
	}
	return resp, http.StatusOK, nil
}
