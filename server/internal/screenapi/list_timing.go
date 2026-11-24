package screenapi

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// listSlowLog is the duration from which a full list response is written to the log.
const listSlowLog = 2 * time.Second

// listKeep is the number of measurements held per kind. They are kept in memory
// and reset on restart; only fixed names and numbers are held, never row content.
const listKeep = 20

var listTimingNames = []string{"issue_read", "closeRequest_read", "draft_read", "todo_read", "reviews_read", "fullTests_read", "stalls_read", "attentionAlerts_read", "hosts_read", "snapshot_prepare", "delta", "json"}

var attentionTimingNames = []string{"drafts", "saved", "notices", "stage", "incidents", "jobs", "jobRecovery", "checkout", "rework", "rc", "visible"}

func init() {
	for _, name := range attentionTimingNames {
		listTimingNames = append(listTimingNames, "attentionAlerts_"+name)
	}
}

type attentionTimingKey struct{}

// ReportAttentionTiming records only known Attention stages for a list request.
func ReportAttentionTiming(ctx context.Context, name string, duration time.Duration) {
	if report, ok := ctx.Value(attentionTimingKey{}).(func(string, time.Duration)); ok {
		report(name, duration)
	}
}

// listClientFields are the numbers a browser may report about showing the list.
var listClientFields = map[string]bool{
	"fetchMs": true, "ttfbMs": true, "integrateMs": true, "drawMs": true, "firstDisplayMs": true, "paintMs": true,
	"fromNavigationMs": true, "encodedBytes": true, "decodedBytes": true, "rows": true,
}

type listTiming struct {
	start    time.Time
	stages   map[string]time.Duration
	snapshot time.Duration
	full     bool
	scope    string
	rows     int
}

func newListTiming() *listTiming {
	return &listTiming{start: time.Now(), stages: map[string]time.Duration{}}
}

func milliseconds(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Millisecond)*1000) / 1000
}

func (t *listTiming) header() string {
	parts := make([]string, 0, len(listTimingNames))
	for _, name := range listTimingNames {
		parts = append(parts, fmt.Sprintf("%s;dur=%.3f", name, milliseconds(t.stages[name])))
	}
	return strings.Join(parts, ", ")
}

func (t *listTiming) observe(value any) {
	if body, ok := value.(Object); ok {
		t.full, _ = body["full"].(bool)
		if rows, ok := body["rows"].([]Row); ok {
			t.rows = len(rows)
		} else {
			t.rows = len(fleet.Items(body["rows"]))
		}
	}
}

// listResponseWriter counts the bytes the server produced, before the entry compresses them.
type listResponseWriter struct {
	http.ResponseWriter
	bytes  int
	status int
}

func (w *listResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *listResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

// ListMeasurements holds recent /api/list measurements for GET /api/meta.
type ListMeasurements struct {
	mu                  sync.Mutex
	full, delta, client []Object
}

func keepLatest(rows []Object, row Object) []Object {
	rows = append(rows, row)
	if len(rows) > listKeep {
		rows = rows[len(rows)-listKeep:]
	}
	return rows
}

func (m *ListMeasurements) server(at time.Time, t *listTiming, w *listResponseWriter) {
	total := time.Since(t.start)
	stages := Object{}
	for _, name := range listTimingNames {
		stages[name] = milliseconds(t.stages[name])
	}
	row := Object{"at": at.UTC().Format(time.RFC3339), "status": w.status, "bytes": w.bytes, "rows": t.rows, "totalMs": milliseconds(total), "stages": stages, "scope": t.scope}
	m.mu.Lock()
	if t.full {
		m.full = keepLatest(m.full, row)
	} else {
		m.delta = keepLatest(m.delta, row)
	}
	m.mu.Unlock()
	if t.full && total >= listSlowLog {
		args := []any{"total_ms", milliseconds(total), "bytes", w.bytes, "rows", t.rows}
		for _, name := range listTimingNames {
			args = append(args, name+"_ms", milliseconds(t.stages[name]))
		}
		slog.Warn("一覧の処理に時間がかかりました", args...)
	}
}

func (m *ListMeasurements) browser(at time.Time, data Object) *contract.Problem {
	row := Object{"at": at.UTC().Format(time.RFC3339)}
	args := []any{}
	for key, value := range data {
		raw, ok := value.(json.Number)
		if !ok || !listClientFields[key] {
			return fail(400, "一覧の計測の項目が不正です。")
		}
		number, e := raw.Float64()
		if e != nil || number < 0 || number > 1e12 {
			return fail(400, "一覧の計測の項目が不正です。")
		}
		row[key] = number
		args = append(args, key, number)
	}
	m.mu.Lock()
	m.client = keepLatest(m.client, row)
	m.mu.Unlock()
	slog.Info("一覧の表示を計測しました", args...)
	return nil
}

func (m *ListMeasurements) snapshot() Object {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Object{
		"source":  "server-memory",
		"full":    append([]Object{}, m.full...),
		"delta":   append([]Object{}, m.delta...),
		"browser": append([]Object{}, m.client...),
	}
}
