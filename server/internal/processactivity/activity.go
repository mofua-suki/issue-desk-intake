// Package processactivity collects bounded, process-local execution statistics.
package processactivity

import (
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"issue-desk-intake/server/internal/contract"
)

const RowsPerHour = 1000

var uuid = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var digits = regexp.MustCompile(`^[0-9]+$`)

type Row struct {
	Kind       string        `json:"kind"`
	Name       string        `json:"name"`
	Method     string        `json:"method,omitempty"`
	Status     string        `json:"status,omitempty"`
	Credential string        `json:"credential,omitempty"`
	Count      int64         `json:"count"`
	Failures   int64         `json:"failures"`
	Total      time.Duration `json:"total"`
	Max        time.Duration `json:"max"`
}
type Hour struct {
	Start time.Time `json:"start"`
	Rows  []Row     `json:"rows"`
}
type Collector struct {
	mu    sync.Mutex
	Now   func() time.Time
	hours map[int64]map[string]*Row
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
func Normalize(path string) string {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" {
		return "invalid-route"
	}
	parts := strings.Split(u.Path, "/")
	for i, p := range parts {
		if digits.MatchString(p) {
			parts[i] = "{n}"
		} else if uuid.MatchString(p) {
			parts[i] = "{id}"
		}
	}
	result := contract.MaskCredentials(strings.Join(parts, "/"))
	if len(result) > 512 {
		return "invalid-route"
	}
	return result
}
func (c *Collector) record(row Row, elapsed time.Duration, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().Truncate(time.Hour).Unix()
	if c.hours == nil {
		c.hours = make(map[int64]map[string]*Row)
	}
	for h := range c.hours {
		if h <= now-24*3600 || h > now {
			delete(c.hours, h)
		}
	}
	bucket := c.hours[now]
	if bucket == nil {
		bucket = make(map[string]*Row)
		c.hours[now] = bucket
	}
	key := strings.Join([]string{row.Kind, row.Name, row.Method, row.Status, row.Credential}, "\x00")
	r := bucket[key]
	if r == nil {
		count := 0
		for _, existing := range bucket {
			if existing.Kind == row.Kind {
				count++
			}
		}
		if count >= RowsPerHour {
			return
		}
		r = &row
		bucket[key] = r
	}
	r.Count++
	if failed {
		r.Failures++
	}
	if elapsed < 0 {
		elapsed = 0
	}
	r.Total += elapsed
	if elapsed > r.Max {
		r.Max = elapsed
	}
}
func (c *Collector) Periodic(name string, elapsed time.Duration, failed bool) {
	c.record(Row{Kind: "periodic", Name: name}, elapsed, failed)
}
func (c *Collector) HTTP(method, path string, status int, credential string, elapsed time.Duration) {
	// Bound attacker-controlled dimensions; never retain request headers or arbitrary methods.
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		method = "OTHER"
	}
	class := "other"
	if status >= 100 && status <= 599 {
		class = string(rune('0'+status/100)) + "xx"
	}
	switch credential {
	case "pc", "cli", "session":
	default:
		credential = "none"
	}
	c.record(Row{Kind: "http", Name: Normalize(path), Method: method, Status: class, Credential: credential}, elapsed, status >= 400)
}
func (c *Collector) Report() []Hour {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().Truncate(time.Hour).Unix()
	hours := make([]Hour, 0, len(c.hours))
	for h, bucket := range c.hours {
		if h <= now-24*3600 || h > now {
			delete(c.hours, h)
			continue
		}
		v := Hour{Start: time.Unix(h, 0).UTC(), Rows: make([]Row, 0, len(bucket))}
		for _, r := range bucket {
			v.Rows = append(v.Rows, *r)
		}
		sort.Slice(v.Rows, func(i, j int) bool {
			a, b := v.Rows[i], v.Rows[j]
			return strings.Join([]string{a.Kind, a.Name, a.Method, a.Status, a.Credential}, "\x00") < strings.Join([]string{b.Kind, b.Name, b.Method, b.Status, b.Credential}, "\x00")
		})
		hours = append(hours, v)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].Start.Before(hours[j].Start) })
	return hours
}

var Process = &Collector{}

func credential(r *http.Request) string {
	if _, ok := r.Header[http.CanonicalHeaderKey("Authorization")]; ok {
		return "pc"
	}
	if c, err := r.Cookie("desk_session"); err == nil && c.Value != "" {
		return "session"
	}
	if _, ok := r.Header[http.CanonicalHeaderKey("X-Desk-Key")]; ok {
		return "cli"
	}
	return "none"
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != 101 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}
func (c *Collector) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		kind := credential(r)
		rw := &responseWriter{ResponseWriter: w}
		defer func() {
			status := rw.status
			if status == 0 {
				status = 200
			}
			c.HTTP(r.Method, r.URL.EscapedPath(), status, kind, time.Since(start))
		}()
		next.ServeHTTP(rw, r)
	})
}
