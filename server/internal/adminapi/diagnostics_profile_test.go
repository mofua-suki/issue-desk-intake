package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

func TestProfileEndpoints(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/api/diagnostics/cpu", "/api/diagnostics/goroutines"} {
		for _, tc := range []struct {
			credential string
			want       int
		}{{"", 401}, {"worker", 403}, {"screen", 403}} {
			code, v := f.call("GET", path, tc.credential, nil)
			status(t, code, tc.want, v)
		}
		code, v := f.call("POST", path, "delegate", nil)
		status(t, code, 405, v)
	}
	for _, query := range []string{"0", "31", "bad", ""} {
		code, v := f.call("GET", "/api/diagnostics/cpu?seconds="+query, "delegate", nil)
		status(t, code, 400, v)
	}
	var before int
	if err := f.db.DB().QueryRow("SELECT count(*) FROM admin_audit").Scan(&before); err != nil {
		t.Fatal(err)
	}
	code, v := f.call("GET", "/api/diagnostics/goroutines", "cli", nil)
	status(t, code, 200, v)
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "screen-secret") || strings.Contains(string(b), f.token["delegate"]) {
		t.Fatal("機密が含まれます")
	}
	if len(obj(v)["functions"].([]any)) == 0 || !strings.Contains(string(b), ".go:") {
		t.Fatal("goroutineの関数がありません")
	}
	if obj(v)["instanceId"] != diagnosticInstance {
		t.Fatal("受付識別子が異なります")
	}
	code, v = f.call("GET", "/api/diagnostics/cpu?seconds=1", "delegate", nil)
	status(t, code, 200, v)
	if obj(v)["instanceId"] != diagnosticInstance || obj(v)["startedAt"] == nil || obj(v)["color"] == nil {
		t.Fatal("CPU応答の受付識別が不正です", v)
	}
	var after int
	if err := f.db.DB().QueryRow("SELECT count(*) FROM admin_audit").Scan(&after); err != nil || before != after {
		t.Fatal("監査状態が変化しました", err)
	}
}

func TestProfileGoroutineLabelsArePrivate(t *testing.T) {
	ready := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan struct{})
	go pprof.Do(context.Background(), pprof.Labels("private", "secret-label-value"), func(context.Context) {
		close(ready)
		<-stop
		close(done)
	})
	<-ready
	defer func() { close(stop); <-done }()
	v, code, err := (&service{}).diagnosticsGoroutines(httptest.NewRequest("GET", "/api/diagnostics/goroutines", nil), auth.Principal{})
	if err != nil || code != 200 {
		t.Fatal(code, err)
	}
	b, err := json.Marshal(v)
	if err != nil || strings.Contains(string(b), "secret-label-value") || !strings.Contains(string(b), "TestProfileGoroutineLabelsArePrivate") {
		t.Fatalf("ラベルまたは先頭フレームが不正: %s (%v)", b, err)
	}
}
func TestProfileConcurrencyAndCancellation(t *testing.T) {
	s := &service{}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/diagnostics/cpu?seconds=30", nil).WithContext(ctx)
	done := make(chan error, 1)
	go func() { _, _, err := s.diagnosticsCPU(req, auth.Principal{}); done <- err }()
	deadline := time.Now().Add(3 * time.Second)
	for len(cpuProfileSlot) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(cpuProfileSlot) == 0 {
		cancel()
		t.Fatal("取得が開始しません")
	}
	_, _, err := s.diagnosticsCPU(httptest.NewRequest("GET", "/api/diagnostics/cpu?seconds=1", nil), auth.Principal{})
	if err == nil || err.(*contract.Problem).Status != 409 {
		t.Fatal("同時取得を拒否しません", err)
	}
	cancel()
	<-done
	_, status, err := s.diagnosticsCPU(httptest.NewRequest("GET", "/api/diagnostics/cpu?seconds=1", nil), auth.Principal{})
	if err != nil || status != 200 {
		t.Fatal("再取得に失敗", status, err)
	}
	_, status, err = s.diagnosticsCPU(httptest.NewRequest("GET", "/api/diagnostics/cpu?seconds=1", nil), auth.Principal{})
	if err != nil || status != 200 {
		t.Fatal("完了後の再取得に失敗", status, err)
	}
}

func TestProfileDoesNotBlockBusinessAPI(t *testing.T) {
	f := newFixture(t)
	done := make(chan int, 1)
	go func() {
		code, _ := f.call("GET", "/api/diagnostics/cpu?seconds=1", "delegate", nil)
		done <- code
	}()
	deadline := time.Now().Add(3 * time.Second)
	for len(cpuProfileSlot) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(cpuProfileSlot) == 0 {
		t.Fatal("CPU取得が開始しません")
	}
	code, v := f.call("GET", "/api/decisions", "cli", nil)
	status(t, code, 200, v)
	select {
	case code = <-done:
		t.Fatalf("業務APIの応答前にCPU取得が終了しました: %d", code)
	default:
	}
	if code = <-done; code != 200 {
		t.Fatal("CPU取得に失敗しました", code)
	}
}

func TestProfileActualSamplesAndIndependentGoroutines(t *testing.T) {
	s := &service{}
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				profileBusyWork()
			}
		}
	}()
	defer close(stop)
	done := make(chan any, 1)
	go func() {
		v, _, err := s.diagnosticsCPU(httptest.NewRequest("GET", "/api/diagnostics/cpu?seconds=1", nil), auth.Principal{})
		if err != nil {
			done <- err
		} else {
			done <- v
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for len(cpuProfileSlot) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, code, err := s.diagnosticsGoroutines(httptest.NewRequest("GET", "/api/diagnostics/goroutines", nil), auth.Principal{})
	if err != nil || code != 200 {
		t.Fatal("並行する診断に失敗", code, err)
	}
	v := <-done
	result, ok := v.(map[string]any)
	if !ok {
		t.Fatal(v)
	}
	if result["totalSamples"].(int64) == 0 {
		t.Fatal("CPU標本が空です")
	}
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), "profileBusyWork") {
		t.Fatalf("実際のCPU消費関数がありません: %s", raw)
	}
	located := false
	for _, entry := range result["self"].([]profileEntry) {
		if strings.Contains(entry.Function, "profileBusyWork") && strings.Contains(entry.File, "diagnostics_profile_test.go:") {
			located = true
		}
	}
	if !located {
		t.Fatalf("CPU関数のファイル行がありません: %s", raw)
	}
}

//go:noinline
func profileBusyWork() {
	for i := 0; i < 10000; i++ {
		profileSink += i
	}
}

var profileSink int

func TestProfileExternalProfilerConflict(t *testing.T) {
	var buffer bytes.Buffer
	if err := pprof.StartCPUProfile(&buffer); err != nil {
		t.Skip("別のCPUプロファイルが実行中:", err)
	}
	s := &service{}
	_, _, err := s.diagnosticsCPU(httptest.NewRequest("GET", "/api/diagnostics/cpu?seconds=1", nil), auth.Principal{})
	pprof.StopCPUProfile()
	if err == nil || err.(*contract.Problem).Status != 409 {
		t.Fatal("外部プロファイルとの競合を拒否しません", err)
	}
	_, status, err := s.diagnosticsCPU(httptest.NewRequest("GET", "/api/diagnostics/cpu?seconds=1", nil), auth.Principal{})
	if err != nil || status != 200 {
		t.Fatal("競合後に再取得できません", status, err)
	}
}

func TestProfileLimitAndIdentity(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ISSUE_DESK_COLOR", "blue")
	for _, endpoint := range []string{"cpu", "goroutines"} {
		for _, query := range []string{"limit=", "limit=0", "limit=101", "limit=bad", "limit=1&limit=2"} {
			code, value := f.call("GET", "/api/diagnostics/"+endpoint+"?"+query, "delegate", nil)
			status(t, code, 400, value)
		}
		query := "?limit=1"
		if endpoint == "cpu" {
			query += "&seconds=1"
		}
		code, value := f.call("GET", "/api/diagnostics/"+endpoint+query, "delegate", nil)
		status(t, code, 200, value)
		result := obj(value)
		if result["color"] != "blue" || result["instanceId"] != diagnosticInstance || result["startedAt"] != diagnosticStarted.Format(time.RFC3339Nano) {
			t.Fatalf("identity: %v", result)
		}
		keys := []string{"functions"}
		if endpoint == "cpu" {
			keys = []string{"self", "cumulative"}
		}
		for _, key := range keys {
			if len(result[key].([]any)) > 1 {
				t.Fatalf("limit ignored: %v", result)
			}
		}
	}
}

func TestCancelledProfileDoesNotWriteFailure(t *testing.T) {
	f := newFixture(t)
	messages := make(chan string, 1)
	f.options.Log = func(message string) { messages <- message }
	f.restart()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", "http://127.0.0.1:8787/api/diagnostics/cpu?seconds=30", nil).WithContext(ctx)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Set("Authorization", "Bearer "+f.token["delegate"])
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.mux.ServeHTTP(response, request) }()
	deadline := time.After(3 * time.Second)
	for len(cpuProfileSlot) == 0 {
		select {
		case <-done:
			t.Fatalf("profile did not start: status=%d body=%s", response.Code, response.Body)
		case <-deadline:
			t.Fatal("profile start timeout")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not release handler")
	}
	if response.Body.Len() != 0 {
		t.Fatalf("wrote response to disconnected request: %s", response.Body)
	}
	select {
	case message := <-messages:
		if !strings.Contains(message, "GET /api/diagnostics/cpu") {
			t.Fatal(message)
		}
	default:
		t.Fatal("missing cancellation log")
	}
	code, value := f.call("GET", "/api/diagnostics/cpu?seconds=1", "delegate", nil)
	status(t, code, 200, value)
}
