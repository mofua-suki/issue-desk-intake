package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type failureScope struct {
	Step     string `json:"step"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type failureSample struct {
	At      float64 `json:"at"`
	Outcome string  `json:"outcome"`
	Cause   string  `json:"cause"`
}
type failureIncident struct {
	ID          string         `json:"id"`
	Detected    float64        `json:"detectedAt"`
	Count       int            `json:"count"`
	Summary     string         `json:"summary"`
	Explanation string         `json:"explanation"`
	Notified    *float64       `json:"notifiedAt,omitempty"`
	Push        map[string]any `json:"push,omitempty"`
	Resolved    *float64       `json:"resolvedAt,omitempty"`
}
type failureRange struct {
	Scope    failureScope     `json:"scope"`
	Samples  []failureSample  `json:"samples"`
	Incident *failureIncident `json:"incident,omitempty"`
	Last     *failureIncident `json:"lastIncident,omitempty"`
}
type failureRecord struct {
	IdleTimeouts []idleTimeoutRecord      `json:"idleTimeouts,omitempty"`
	Version      int                      `json:"version"`
	Ranges       map[string]*failureRange `json:"ranges"`
}
type AIFailures struct {
	mu                   sync.Mutex
	file                 string
	record               failureRecord
	loadErr              error
	ctx                  context.Context
	cancel               context.CancelFunc
	notify               func(context.Context, map[string]any) (map[string]any, error)
	wg                   sync.WaitGroup
	closed               bool
	notificationsStopped bool
}

var failureCauses = map[string]string{"invalid_json_schema": "構造化出力のスキーマが不正です", "authentication": "認証に失敗しました", "connection": "接続に失敗しました", "timeout": "実行が時間切れになりました", "stalled": "実行の出力が途絶えました", "aborted": "理由を確認できない中断です", "execution": "AIの実行に失敗しました"}

func NewAIFailures(ctx context.Context, runtime string, notify func(context.Context, map[string]any) (map[string]any, error)) *AIFailures {
	child, cancel := context.WithCancel(ctx)
	s := &AIFailures{ctx: child, cancel: cancel, file: filepath.Join(runtime, "state", "intake-ai-failures.json"), record: failureRecord{Version: 1, Ranges: map[string]*failureRange{}}, notify: notify}
	raw, e := os.ReadFile(s.file)
	if os.IsNotExist(e) {
		return s
	}
	if e != nil {
		s.loadErr = e
		return s
	}
	var r failureRecord
	if len(raw) > 4*1024*1024 || !validFailureJSON(raw) || json.Unmarshal(raw, &r) != nil || r.Version != 1 || r.Ranges == nil {
		s.loadErr = fmt.Errorf("AIの障害記録を読めません")
		return s
	}
	for _, r := range r.Ranges {
		if !validFailureRange(r) {
			s.loadErr = fmt.Errorf("AIの障害記録の形式を確認できません")
			return s
		}
	}
	for _, event := range r.IdleTimeouts {
		if !validIdleTimeout(event) {
			s.loadErr = fmt.Errorf("AIの障害記録の形式を確認できません")
			return s
		}
	}
	s.record = r
	return s
}
func validFailureRange(r *failureRange) bool {
	if r == nil || r.Samples == nil {
		return false
	}
	for _, sample := range r.Samples {
		if !validFailureTime(sample.At) || (sample.Outcome != "success" && sample.Outcome != "input_invalid" && sample.Outcome != "failure") {
			return false
		}
	}
	for _, incident := range []*failureIncident{r.Incident, r.Last} {
		if incident == nil {
			continue
		}
		if incident.ID == "" || !validFailureTime(incident.Detected) || incident.Count < 0 {
			return false
		}
		if incident.Notified != nil && !validFailureTime(*incident.Notified) {
			return false
		}
		if incident.Resolved != nil && !validFailureTime(*incident.Resolved) {
			return false
		}
		if incident.Push != nil {
			state := incident.Push["state"]
			if state != "attempted" && state != "returned" && state != "failed" {
				return false
			}
		}
	}
	return true
}

// The prior JSON contract accepts every finite JavaScript Date millisecond,
// including fractional values. Keep that precision until formatting a Date.
func validFailureTime(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Abs(value) <= 8.64e15
}

var safeFailureID = regexp.MustCompile(`[^\pL\pN ._:/\[\]-]`)

func failureID(v string) string { return safeFailureID.ReplaceAllString(usage.Redact(v), "_") }
func (s *AIFailures) Observe(o ai.Observation) {
	outcome, cause, causeKey, counted := classifyFailure(o)
	if !counted {
		return
	}
	if o.StartedAt.IsZero() || o.FinishedAt.IsZero() {
		return
	}
	key := failureScopeKey(o)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	r := s.record.Ranges[key]
	if r == nil {
		r = &failureRange{Scope: failureScope{failureID(o.Step), failureID(o.Provider), failureID(o.Model)}, Samples: []failureSample{}}
		s.record.Ranges[key] = r
	}
	at := float64(o.FinishedAt.UnixMilli())
	if o.DiagnosticKind == "timeout_idle" {
		s.addIdleTimeout(o, at)
	}
	r.Samples = append(recentFailureSamples(r.Samples, at), failureSample{at, outcome, causeKey})
	if outcome == "success" {
		if r.Incident != nil && float64(o.StartedAt.UnixMilli()) > r.Incident.Detected {
			r.Last = r.Incident
			r.Last.Resolved = &at
			r.Incident = nil
			r.Samples = []failureSample{}
		}
		_ = s.save()
		s.mu.Unlock()
		return
	}
	detected := failureDetected(r.Samples, outcome, causeKey)
	if r.Incident == nil && detected {
		id, e := requestID()
		if e != nil {
			s.mu.Unlock()
			return
		}
		count := 0
		for _, sample := range r.Samples {
			if sample.Outcome != "success" {
				count++
			}
		}
		r.Incident = &failureIncident{ID: id, Detected: at, Count: count, Summary: failureCauses[cause]}
	} else if r.Incident != nil {
		r.Incident.Count++
	}
	var send *failureIncident
	if r.Incident != nil {
		r.Incident.Summary = failureCauses[cause]
		r.Incident.Explanation = fmt.Sprintf("対象結果の失敗%d件。%s。同じ工程・実行先・モデルの検知後に開始した呼び出しの成功で解消します。", r.Incident.Count, failureCauses[cause])
		if r.Incident.Notified == nil && !s.notificationsStopped {
			r.Incident.Notified = &at
			r.Incident.Push = map[string]any{"state": "attempted"}
			if s.save() == nil {
				send = r.Incident
			} else {
				r.Incident.Notified = nil
				r.Incident.Push = nil
			}
		}
	}
	_ = s.save()
	if send != nil && s.notify != nil {
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if send != nil && s.notify != nil {
		go s.send(send)
	}
}

// classifyFailure は呼び出しの結果を成功・失敗・入力不正と原因に分類する。意図した停止や利用上限など数えない結果は counted=false を返す。
func classifyFailure(o ai.Observation) (outcome, cause, causeKey string, counted bool) {
	outcome, cause = o.Outcome, o.Cause
	if outcome == "canceled" || cause == "intentional_stop" || cause == "service_shutdown" || cause == "rate_limit" || cause == "usage_limit" {
		return "", "", "", false
	}
	if outcome != "success" {
		outcome = "failure"
	}
	if cause == "input_invalid" || cause == "schema" || cause == "input_size" {
		outcome, cause = "input_invalid", "invalid_json_schema"
	}
	if _, ok := failureCauses[cause]; !ok {
		cause = "execution"
	}
	causeKey = o.CauseKey
	if !contractHash.MatchString(causeKey) {
		causeKey = cause
	}
	return outcome, cause, causeKey, true
}

// failureScopeKey は工程・実行先・モデルの組から、失敗を数える範囲の鍵を作る。
func failureScopeKey(o ai.Observation) string {
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	_ = enc.Encode([]string{o.Step, o.Provider, o.Model})
	sum := sha256.Sum256(bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}))
	return hex.EncodeToString(sum[:])
}

// recentFailureSamples は直近15分の結果を、新しい結果を足しても20件に収まるように残す。
func recentFailureSamples(previous []failureSample, at float64) []failureSample {
	samples := []failureSample{}
	for _, sample := range previous {
		if sample.At >= at-15*60000 {
			samples = append(samples, sample)
		}
	}
	if len(samples) > 19 {
		samples = samples[len(samples)-19:]
	}
	return samples
}

// failureDetected は直近の結果がすべて失敗か、最後の3件が同じ原因の失敗か、入力不正なら有事と判定する。
func failureDetected(samples []failureSample, outcome, causeKey string) bool {
	allFailed := len(samples) >= 3
	for _, sample := range samples {
		if sample.Outcome == "success" {
			allFailed = false
		}
	}
	tailSame := len(samples) >= 3
	if tailSame {
		for _, sample := range samples[len(samples)-3:] {
			if sample.Outcome == "success" || sample.Cause != causeKey {
				tailSame = false
			}
		}
	}
	return outcome == "input_invalid" || allFailed || tailSame
}

func (s *AIFailures) save() error {
	if s.loadErr != nil {
		return s.loadErr
	}
	raw, e := json.Marshal(s.record)
	if e != nil {
		return e
	}
	directory := filepath.Dir(s.file)
	if e = os.MkdirAll(directory, 0700); e != nil {
		return e
	}
	id, e := requestID()
	if e != nil {
		return e
	}
	temporary := s.file + "." + id + ".tmp"
	defer os.Remove(temporary)
	file, e := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = file.Write(raw)
	if e == nil {
		e = file.Sync()
	}
	ce := file.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(temporary, s.file); e != nil {
		return e
	}
	dir, e := os.Open(directory)
	if e != nil {
		return e
	}
	e = dir.Sync()
	dir.Close()
	return e
}
func (s *AIFailures) send(incident *failureIncident) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	result, e := s.notify(ctx, map[string]any{"title": "開発受付", "body": "受付AIの共通障害を検知しました。記録を確認してください。", "url": "/"})
	s.mu.Lock()
	defer s.mu.Unlock()
	incident.Push = map[string]any{"state": "returned"}
	if e != nil {
		incident.Push["state"] = "failed"
	} else {
		for _, key := range []string{"sent", "failed", "subscriptions"} {
			if value, exists := result[key]; exists {
				switch n := value.(type) {
				case float64:
					if n >= 0 && n == float64(int64(n)) {
						incident.Push[key] = n
					}
				case int:
					if n >= 0 {
						incident.Push[key] = n
					}
				}
			}
		}
	}
	_ = s.save()
}
func (s *AIFailures) Alerts(context.Context) ([]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, fmt.Errorf("AIの障害記録を確認できません")
	}
	out := []any{}
	if alert := s.idleTimeoutAlert(time.Now()); alert != nil {
		out = append(out, alert)
	}
	for _, r := range s.record.Ranges {
		if r.Incident == nil {
			continue
		}
		i := r.Incident
		out = append(out, map[string]any{"id": "intake-ai-" + i.ID, "kind": "intake_ai_failure", "status": "stall_alert", "project": fleet.SystemProject, "detectedAt": iso(time.UnixMilli(int64(i.Detected))), "title": "受付AIの共通障害: " + failureID(r.Scope.Step), "detail": strings.Join([]string{failureID(r.Scope.Provider) + " / " + failureID(r.Scope.Model), i.Explanation}, "\n")})
	}
	return out, nil
}
func (s *AIFailures) Close() { s.mu.Lock(); s.closed = true; s.cancel(); s.mu.Unlock(); s.wg.Wait() }
