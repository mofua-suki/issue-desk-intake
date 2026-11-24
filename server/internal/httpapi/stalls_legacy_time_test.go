package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"issue-desk-intake/server/internal/operations"
)

func TestHTTPStallsReadsLegacyFractionalTimesAndRejectsInvalidRecord(t *testing.T) {
	legacy, err := os.ReadFile("../operations/testdata/ai-failures-legacy-fractional.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		raw  []byte
		code int
	}{
		{"旧正規の小数時刻", legacy, 200},
		{"範囲外の時刻を空成功にしない", []byte(strings.ReplaceAll(string(legacy), "1793491200000.125", "8640000000000001")), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var file string
			var notified atomic.Int32
			handler, _, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
				file = filepath.Join(o.RuntimeDir, "state", "intake-ai-failures.json")
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, tc.raw, 0600); err != nil {
					t.Fatal(err)
				}
				observer := operations.NewAIFailures(context.Background(), o.RuntimeDir, func(context.Context, map[string]any) (map[string]any, error) {
					notified.Add(1)
					return nil, nil
				})
				t.Cleanup(observer.Close)
				o.AIAlerts = observer.Alerts
			})
			r := httptest.NewRequest("GET", "http://localhost:8787/api/stalls/alerts", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatal(w.Code, w.Body.String())
			}
			if tc.code == 200 {
				var rows []map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows {
					if row["id"] == "intake-ai-legacy-reserved" {
						found = row["detectedAt"] == "2026-11-01T00:00:00.000Z"
					}
				}
				if !found {
					t.Fatal("旧通知を保持していません", rows)
				}
			}
			saved, err := os.ReadFile(file)
			if err != nil || string(saved) != string(tc.raw) || notified.Load() != 0 {
				t.Fatal("HTTP読取で原文または通知予約を変更", err, notified.Load())
			}
		})
	}
}
