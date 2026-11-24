package ai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"issue-desk-intake/server/internal/contract"
	"math"
	"os"
	"regexp"
	"sort"
	"time"
)

// LimitObservation contains only the allowlisted, non-conversation limit fields.
type LimitObservation map[string]any

var limitName = regexp.MustCompile(`^[a-zA-Z0-9_]{1,80}$`)

func safeLimitText(v any) any {
	s, ok := v.(string)
	if !ok || !limitName.MatchString(s) {
		return nil
	}
	return s
}
func limitNumber(v any) any {
	n := amount(v)
	if n == nil {
		return nil
	}
	return *n
}
func limitTime(v any) any {
	var t time.Time
	switch x := v.(type) {
	case json.Number:
		f, e := x.Float64()
		if e != nil || f < 0 || f > 253402300799 {
			return nil
		}
		t = time.Unix(int64(f), 0)
	case string:
		var e error
		t, e = time.Parse(time.RFC3339Nano, x)
		if e != nil {
			return nil
		}
	default:
		return nil
	}
	return contract.ISO(t)
}
func getField(m map[string]any, names ...string) any {
	for _, n := range names {
		if v, ok := m[n]; ok {
			return v
		}
	}
	return nil
}
func claudeLimits(data []byte) []LimitObservation {
	last := map[string]LimitObservation{}
	for _, event := range events(data) {
		if event["type"] != "rate_limit_event" {
			continue
		}
		info := object(getField(event, "rate_limit_info", "rateLimitInfo"))
		if info == nil {
			continue
		}
		windows := object(getField(info, "unifiedWindows", "unified_windows"))
		if windows == nil {
			name := safeLimitText(getField(info, "rateLimitType", "rate_limit_type"))
			key, _ := name.(string)
			last[key] = claudeWindow(name, info)
		} else {
			for key, value := range windows {
				name := safeLimitText(key)
				if name == nil {
					continue
				}
				last[key] = claudeWindow(name, object(value))
			}
		}
	}
	keys := make([]string, 0, len(last))
	for k := range last {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]LimitObservation, 0, len(keys))
	for _, k := range keys {
		out = append(out, last[k])
	}
	return out
}
func claudeWindow(name any, window map[string]any) LimitObservation {
	u := limitNumber(window["utilization"])
	var percent any
	if v, ok := u.(float64); ok && !math.IsInf(v*100, 0) {
		percent = v * 100
		if v <= 1e6 {
			percent = math.Round(v*100*1e10) / 1e10
		}
	}
	return LimitObservation{"provider": "claude", "limit": name, "status": safeLimitText(window["status"]), "utilization": u, "percent": percent, "resetsAt": limitTime(getField(window, "resetsAt", "resets_at")), "windowMinutes": nil}
}
func codexLimits(data []byte, started time.Time) ([]LimitObservation, bool) {
	found := false
	var limits map[string]any
	for _, e := range events(data) {
		p := object(e["payload"])
		if e["type"] != "event_msg" || p["type"] != "token_count" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, stringValue(e["timestamp"]))
		if err != nil || at.Before(started) {
			continue
		}
		found = true
		limits = object(p["rate_limits"])
	}
	var out []LimitObservation
	for _, key := range []string{"primary", "secondary"} {
		w := object(limits[key])
		if w == nil {
			continue
		}
		var name, minutes any
		if n := count(w["window_minutes"]); n != nil && *n > 0 {
			minutes = *n
			switch *n {
			case 300:
				name = "five_hour"
			case 10080:
				name = "seven_day"
			default:
				name = fmt.Sprintf("window_%dm", *n)
			}
		}
		out = append(out, LimitObservation{"provider": "codex", "limit": name, "status": nil, "utilization": nil, "percent": limitNumber(w["used_percent"]), "resetsAt": limitTime(w["resets_at"]), "windowMinutes": minutes})
	}
	return out, found
}
func stringValue(v any) string { s, _ := v.(string); return s }
func rolloutLimits(id string, started time.Time) []LimitObservation {
	f, err := os.Open(rolloutPath(id))
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	reader := bufio.NewReader(io.LimitReader(f, info.Size()))
	var latest []LimitObservation
	for {
		line, err := readRolloutLine(reader)
		if rows, found := codexLimits(line, started); found {
			latest = rows
		}
		if err == io.EOF {
			return latest
		}
		if err != nil {
			return nil
		}
	}
}
