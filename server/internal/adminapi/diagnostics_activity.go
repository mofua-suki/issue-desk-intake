package adminapi

import (
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/processactivity"
	"net/http"
)

type activityRow struct {
	Kind              string  `json:"kind"`
	Name              string  `json:"name"`
	Method            string  `json:"method,omitempty"`
	Status            string  `json:"status,omitempty"`
	Credential        string  `json:"credential,omitempty"`
	Count             int64   `json:"count"`
	Failures          int64   `json:"failures"`
	TotalMilliseconds float64 `json:"totalMilliseconds"`
	MaxMilliseconds   float64 `json:"maxMilliseconds"`
}

func (s *service) diagnosticsActivity(r *http.Request, _ auth.Principal) (any, int, error) {
	if r.Method != http.MethodGet {
		return nil, 0, bad(405, "GETだけを使えます。")
	}
	hours := make([]map[string]any, 0)
	for _, h := range processactivity.Process.Report() {
		rows := make([]activityRow, 0, len(h.Rows))
		for _, row := range h.Rows {
			rows = append(rows, activityRow{row.Kind, row.Name, row.Method, row.Status, row.Credential, row.Count, row.Failures, float64(row.Total) / 1e6, float64(row.Max) / 1e6})
		}
		hours = append(hours, map[string]any{"start": h.Start, "rows": rows})
	}
	result := diagnosticIdentity()
	result["hours"] = hours
	return result, 200, nil
}
