package adminapi

import (
	"bufio"
	"bytes"
	"net/http"
	"os"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"time"

	"issue-desk-intake/server/internal/auth"
)

var cpuProfileSlot = make(chan struct{}, 1)

type profileEntry struct {
	Function   string `json:"function"`
	File       string `json:"file,omitempty"`
	Self       int64  `json:"selfNanoseconds,omitempty"`
	Cumulative int64  `json:"cumulativeNanoseconds,omitempty"`
	Count      int64  `json:"count,omitempty"`
}

func diagnosticIdentity() map[string]any {
	color := os.Getenv("ISSUE_DESK_COLOR")
	if color != "blue" && color != "green" {
		color = "unknown"
	}
	return map[string]any{"color": color, "instanceId": diagnosticInstance, "startedAt": diagnosticStarted}
}
func profileLimit(r *http.Request) (int, error) {
	values, exists := r.URL.Query()["limit"]
	if !exists {
		return 30, nil
	}
	if len(values) != 1 {
		return 0, bad(400, "件数は1〜100を1回だけ指定してください。")
	}
	limit, err := strconv.Atoi(values[0])
	if err != nil || limit < 1 || limit > 100 {
		return 0, bad(400, "件数は1〜100を1回だけ指定してください。")
	}
	return limit, nil
}
func (s *service) diagnosticsCPU(r *http.Request, _ auth.Principal) (any, int, error) {
	if r.Method != http.MethodGet {
		return nil, 0, bad(405, "GETだけを使えます。")
	}
	limit, limitErr := profileLimit(r)
	if limitErr != nil {
		return nil, 0, limitErr
	}
	seconds := 10
	if v, ok := r.URL.Query()["seconds"]; ok {
		if len(v) != 1 {
			return nil, 0, bad(400, "秒数が不正です。")
		}
		n, e := strconv.Atoi(v[0])
		if e != nil || n < 1 || n > 30 {
			return nil, 0, bad(400, "秒数は1〜30です。")
		}
		seconds = n
	}
	select {
	case cpuProfileSlot <- struct{}{}:
	default:
		return nil, 0, bad(409, "CPUプロファイル取得中です。")
	}
	defer func() { <-cpuProfileSlot }()
	var b bytes.Buffer
	if err := pprof.StartCPUProfile(&b); err != nil {
		return nil, 0, bad(409, "CPUプロファイルを開始できません。")
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	select {
	case <-timer.C:
	case <-r.Context().Done():
		if !timer.Stop() {
			<-timer.C
		}
		pprof.StopCPUProfile()
		return nil, 0, r.Context().Err()
	}
	pprof.StopCPUProfile()
	self, cumulative, total, err := parseCPUProfile(b.Bytes())
	if err != nil {
		return nil, 0, bad(503, "CPUプロファイルを解析できません。")
	}
	result := diagnosticIdentity()
	result["seconds"] = seconds
	result["totalSamples"] = total
	result["self"] = self[:min(limit, len(self))]
	result["cumulative"] = cumulative[:min(limit, len(cumulative))]
	return result, 200, nil
}
func (s *service) diagnosticsGoroutines(r *http.Request, _ auth.Principal) (any, int, error) {
	if r.Method != http.MethodGet {
		return nil, 0, bad(405, "GETだけを使えます。")
	}
	limit, limitErr := profileLimit(r)
	if limitErr != nil {
		return nil, 0, limitErr
	}
	var b bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&b, 1); err != nil {
		return nil, 0, bad(503, "goroutineを取得できません。")
	}
	scanner := bufio.NewScanner(&b)
	counts := map[string]*profileEntry{}
	var total int64
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, " @ ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, e := strconv.ParseInt(fields[0], 10, 64)
		if e != nil {
			continue
		}
		total += n
		var frame []string
		for scanner.Scan() {
			line = scanner.Text()
			if strings.HasPrefix(line, "#\t0x") {
				frame = strings.Fields(line)
				break
			}
			if line == "" {
				break
			}
		}
		if len(frame) < 4 {
			continue
		}
		fn := frame[2]
		if i := strings.LastIndex(fn, "+"); i >= 0 {
			fn = fn[:i]
		}
		file := frame[3]
		if i := strings.LastIndex(file, "+"); i >= 0 {
			file = file[:i]
		}
		key := fn + "\x00" + file
		if counts[key] == nil {
			counts[key] = &profileEntry{Function: fn, File: file}
		}
		counts[key].Count += n
	}
	if scanner.Err() != nil {
		return nil, 0, bad(503, "goroutineを解析できません。")
	}
	entries := make([]profileEntry, 0, len(counts))
	for _, v := range counts {
		entries = append(entries, *v)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Count == entries[j].Count {
			return entries[i].Function < entries[j].Function
		}
		return entries[i].Count > entries[j].Count
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	result := diagnosticIdentity()
	result["total"] = total
	result["functions"] = entries
	return result, 200, nil
}
