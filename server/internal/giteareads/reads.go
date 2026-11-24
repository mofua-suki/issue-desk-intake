package giteareads

import (
	"issue-desk-intake/server/internal/contract"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type counter struct {
	Route  string `json:"route"`
	Count  int64  `json:"count"`
	Bytes  int64  `json:"bytes"`
	Errors int64  `json:"errors"`
}
type Collector struct {
	mu    sync.Mutex
	Now   func() time.Time
	hours map[int64]map[string]*counter
}

var numbers = regexp.MustCompile(`/[0-9]+(/|$)`)

func Normalize(route string) string {
	// Retain only relative paths and the three bounded, non-secret query fields.
	parsed, e := url.Parse(route)
	if e != nil || parsed.IsAbs() || parsed.Host != "" {
		return "invalid-route"
	}
	path := parsed.EscapedPath()
	for numbers.MatchString(path) {
		path = numbers.ReplaceAllString(path, "/{n}$1")
	}
	q := []string{}
	parts := strings.Split(parsed.RawQuery, "&")
	for _, key := range []string{"labels", "state", "type"} {
		for _, p := range parts {
			k, v, found := strings.Cut(p, "=")
			if k == key {
				if !found {
					v = ""
				}
				q = append(q, k+"="+v)
				break
			}
		}
	}
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	return path
}
func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
func (c *Collector) Record(method, route string, bytes int, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hours == nil {
		c.hours = map[int64]map[string]*counter{}
	}
	hour := c.now().Truncate(time.Hour).UnixMilli()
	bucket := c.hours[hour]
	if bucket == nil {
		bucket = map[string]*counter{}
		c.hours[hour] = bucket
	}
	key := method + " " + Normalize(route)
	r := bucket[key]
	if r == nil {
		r = &counter{Route: key}
		bucket[key] = r
	}
	r.Count++
	if bytes > 0 {
		r.Bytes += int64(bytes)
	}
	if failed {
		r.Errors++
	}
	starts := []int64{}
	for h := range c.hours {
		starts = append(starts, h)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] > starts[j] })
	if len(starts) > 24 {
		for _, h := range starts[24:] {
			delete(c.hours, h)
		}
	}
}
func (c *Collector) Report() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	starts := []int64{}
	for h := range c.hours {
		starts = append(starts, h)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] > starts[j] })
	hours := []any{}
	current := c.now().Truncate(time.Hour).UnixMilli()
	for _, h := range starts {
		rows := []counter{}
		var count, bytes, errors int64
		for _, r := range c.hours[h] {
			rows = append(rows, *r)
			count += r.Count
			bytes += r.Bytes
			errors += r.Errors
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Count != rows[j].Count {
				return rows[i].Count > rows[j].Count
			}
			if rows[i].Bytes != rows[j].Bytes {
				return rows[i].Bytes > rows[j].Bytes
			}
			return rows[i].Route < rows[j].Route
		})
		if len(rows) > 30 {
			rows = rows[:30]
		}
		hours = append(hours, map[string]any{"start": contract.ISO(time.UnixMilli(h)), "end": contract.ISO(time.UnixMilli(h).Add(time.Hour)), "current": h == current, "count": count, "bytes": bytes, "errors": errors, "rows": rows})
	}
	return map[string]any{"hourMs": 3600000, "hoursKept": 24, "rowsPerHour": 30, "hours": hours}
}

var Process = &Collector{}
