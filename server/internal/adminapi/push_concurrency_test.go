package adminapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var parallelNotice = PhoneNotice{Title: "試験", Body: "本文", URL: "/"}

// Exercise the shipped state reader/writer, not an in-memory replacement.
func persistedPushFixture(t testing.TB, count int) (*service, []any) {
	t.Helper()
	s := &service{Options: Options{RuntimeDir: t.TempDir(), Now: time.Now}}
	list := make([]any, count)
	for i := range list {
		list[i] = pushInput(t, fmt.Sprintf("https://fcm.googleapis.com/%d", i))
	}
	if err := s.withPush(func(state map[string]any) (bool, error) {
		state["subscriptions"] = list
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, list
}

func savedPushSubscriptions(t testing.TB, s *service) []any {
	t.Helper()
	var list []any
	if err := s.withPush(func(state map[string]any) (bool, error) {
		list = state["subscriptions"].([]any)
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	return list
}

func pushResponse(code int) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
}

func pushIndex(r *http.Request) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
	return n
}

// The test runner's deadline guards deadlocks; progress assertions use channels.
func pushTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return ctx
}

func awaitPushSignal(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("通知処理が進みませんでした")
	}
}

func TestPushBoundedProgressAndCounts(t *testing.T) {
	for _, count := range []int{0, 1, 32} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			s, _ := persistedPushFixture(t, count)
			ctx := pushTestContext(t)
			entered := make(chan struct{}, 32)
			later := make(chan struct{}, 32)
			fast, slow := make(chan struct{}), make(chan struct{})
			var active, maximum, calls atomic.Int32
			s.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				n := active.Add(1)
				for old := maximum.Load(); n > old; old = maximum.Load() {
					if maximum.CompareAndSwap(old, n) {
						break
					}
				}
				defer active.Add(-1)
				entered <- struct{}{}
				gate := fast
				index := pushIndex(r)
				if index == 0 || index == 2 || index == 3 {
					gate = slow
				}
				select {
				case <-gate:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				if index == 5 {
					// With slots 0/2/3 blocked, starting index 5 proves that
					// index 4's success was fully received and collected.
					later <- struct{}{}
				}
				return pushResponse(201), nil
			})}
			done := make(chan PhonePushResult, 1)
			go func() { done <- s.notifyPhone(ctx, parallelNotice) }()
			for i := 0; i < min(count, 4); i++ {
				awaitPushSignal(t, ctx, entered)
			}
			if got := calls.Load(); got != int32(min(count, 4)) {
				t.Fatalf("開始数=%d", got)
			}
			close(fast)
			// The fifth success is collected before releasing the first destination.
			if count > 4 {
				awaitPushSignal(t, ctx, later)
			}
			close(slow)
			select {
			case result := <-done:
				if result != (PhonePushResult{Sent: count, Subscriptions: count}) {
					t.Fatal(result)
				}
			case <-ctx.Done():
				t.Fatal("送信が回収されませんでした")
			}
			if calls.Load() != int32(count) || active.Load() != 0 || maximum.Load() != int32(min(count, 4)) {
				t.Fatalf("calls=%d active=%d maximum=%d", calls.Load(), active.Load(), maximum.Load())
			}
		})
	}
}

func TestPushCancellationDrainsStartedAndSkipsPending(t *testing.T) {
	s, _ := persistedPushFixture(t, 32)
	ctx, cancel := context.WithCancel(pushTestContext(t))
	defer cancel()
	entered, canceled := make(chan struct{}, 4), make(chan struct{}, 4)
	release := make(chan struct{})
	var calls, active atomic.Int32
	s.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		active.Add(1)
		defer active.Add(-1)
		entered <- struct{}{}
		<-r.Context().Done()
		canceled <- struct{}{}
		<-release // Caller must still wait for these started transports.
		return nil, r.Context().Err()
	})}
	done := make(chan PhonePushResult, 1)
	go func() { done <- s.notifyPhone(ctx, parallelNotice) }()
	guard := pushTestContext(t)
	for i := 0; i < 4; i++ {
		awaitPushSignal(t, guard, entered)
	}
	cancel()
	for i := 0; i < 4; i++ {
		awaitPushSignal(t, guard, canceled)
	}
	select {
	case <-done:
		t.Fatal("開始済みの送信を回収する前に戻りました")
	default:
	}
	close(release)
	select {
	case result := <-done:
		if result != (PhonePushResult{Failed: 32, Subscriptions: 32}) {
			t.Fatal(result)
		}
	case <-guard.Done():
		t.Fatal("キャンセルした送信を回収できませんでした")
	}
	if calls.Load() != 4 || active.Load() != 0 || len(savedPushSubscriptions(t, s)) != 32 {
		t.Fatalf("calls=%d active=%d", calls.Load(), active.Load())
	}
	// Cancellation before entry must also make zero HTTP calls.
	result := s.notifyPhone(ctx, parallelNotice)
	if calls.Load() != 4 || result != (PhonePushResult{Failed: 32, Subscriptions: 32}) {
		t.Fatal(result)
	}
}

func TestPushMixedExpiryTimeoutAndConcurrentUpdates(t *testing.T) {
	s, original := persistedPushFixture(t, 8)
	ctx := pushTestContext(t)
	blocked := make(chan struct{}, 8)
	release := make(chan struct{})
	var calls atomic.Int32
	// Make this update-race scenario independent of filesystem speed. Actual
	// client timeouts are covered by the response-body and performance tests.
	s.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		n := pushIndex(r)
		blocked <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		switch n {
		case 3:
			return nil, context.DeadlineExceeded
		case 0, 5, 6:
			return pushResponse(410), nil
		case 1:
			return pushResponse(404), nil
		case 4:
			return pushResponse(500), nil
		}
		return pushResponse(201), nil
	})}
	// Failure logs are deliberately a plain slice: delivery must not call Log in parallel.
	logs := []string{}
	s.Log = func(message string) { logs = append(logs, message) }
	done := make(chan PhonePushResult, 1)
	go func() { done <- s.notifyPhone(ctx, parallelNotice) }()
	for i := 0; i < 4; i++ {
		awaitPushSignal(t, ctx, blocked)
	}
	// Change auth only, p256dh only, and append another endpoint while HTTP is blocked.
	authReplacement := pushInput(t, str(obj(original[0])["endpoint"]))
	obj(authReplacement["keys"])["p256dh"] = obj(obj(original[0])["keys"])["p256dh"]
	keyReplacement := pushInput(t, str(obj(original[5])["endpoint"]))
	obj(keyReplacement["keys"])["auth"] = obj(obj(original[5])["keys"])["auth"]
	added := pushInput(t, "https://fcm.googleapis.com/added")
	if err := s.withPush(func(state map[string]any) (bool, error) {
		list := state["subscriptions"].([]any)
		list[0], list[5] = authReplacement, keyReplacement
		state["subscriptions"] = append(list, added)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case result := <-done:
		if result != (PhonePushResult{Sent: 2, Failed: 6, Subscriptions: 8}) {
			t.Fatal(result)
		}
	case <-ctx.Done():
		t.Fatal("混在送信が終わりませんでした")
	}
	kept := savedPushSubscriptions(t, s)
	if len(kept) != 7 || calls.Load() != 8 {
		t.Fatalf("kept=%d calls=%d", len(kept), calls.Load())
	}
	byEndpoint := map[string]map[string]any{}
	for _, v := range kept {
		m := obj(v)
		byEndpoint[str(m["endpoint"])] = m
	}
	for _, i := range []int{1, 6} {
		if byEndpoint[str(obj(original[i])["endpoint"])] != nil {
			t.Fatalf("失効購読%dが残りました", i)
		}
	}
	for _, replacement := range []map[string]any{authReplacement, keyReplacement, added} {
		keys := obj(byEndpoint[str(replacement["endpoint"])]["keys"])
		if keys["auth"] != obj(replacement["keys"])["auth"] || keys["p256dh"] != obj(replacement["keys"])["p256dh"] {
			t.Fatal("更新・追加が失われました")
		}
	}
	if len(logs) != 6 {
		t.Fatalf("failure logs=%d", len(logs))
	}
	for _, message := range logs {
		if strings.Contains(message, "fcm.googleapis.com") || strings.Contains(message, str(obj(obj(original[0])["keys"])["auth"])) {
			t.Fatal("購読情報をログに出力しました")
		}
	}
}

func TestPushInvalidSnapshotAndFailedTransportKeepSubscriptions(t *testing.T) {
	s, list := persistedPushFixture(t, 3)
	obj(list[0])["endpoint"] = "https://127.0.0.1/private"
	obj(obj(list[1])["keys"])["p256dh"] = strings.Repeat("a", 87) // Valid shape, invalid EC point.
	if err := s.withPush(func(state map[string]any) (bool, error) { state["subscriptions"] = list; return true, nil }); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("synthetic failure")
	})}
	result := s.notifyPhone(pushTestContext(t), parallelNotice)
	if result != (PhonePushResult{Failed: 3, Subscriptions: 3}) || calls.Load() != 1 || len(savedPushSubscriptions(t, s)) != 3 {
		t.Fatalf("result=%+v calls=%d", result, calls.Load())
	}
}

func TestPushDeliveryDeadlineRedirectAndBodyCleanup(t *testing.T) {
	for _, timeout := range []time.Duration{0, 30 * time.Second, 2 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			s, _ := persistedPushFixture(t, 8)
			var calls, closed atomic.Int32
			client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
				t.Error("注入元クライアントのredirect設定を使いました")
				return nil
			}, Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.URL.Host != "fcm.googleapis.com" {
					t.Error("redirectに従いました")
				}
				deadline, ok := r.Context().Deadline()
				limit := 10 * time.Second
				if timeout > 0 && timeout < limit {
					limit = timeout
				}
				if !ok || time.Until(deadline) > limit {
					t.Error("個別timeoutを維持していません")
				}
				response := pushResponse(302)
				response.Header.Set("Location", "http://127.0.0.1/private")
				response.Body = &countedPushBody{ReadCloser: response.Body, closed: &closed}
				return response, nil
			})}
			s.HTTPClient = client
			result := s.notifyPhone(pushTestContext(t), parallelNotice)
			if result != (PhonePushResult{Failed: 8, Subscriptions: 8}) || calls.Load() != 8 || closed.Load() != 8 || client.Timeout != timeout || client.CheckRedirect == nil {
				t.Fatalf("result=%+v calls=%d closed=%d", result, calls.Load(), closed.Load())
			}
			if len(savedPushSubscriptions(t, s)) != 8 {
				t.Fatal("redirectで購読が削除されました")
			}
		})
	}
}

func TestPushConcurrentInvocationsHaveIndependentLimits(t *testing.T) {
	s, _ := persistedPushFixture(t, 8)
	ctx := pushTestContext(t)
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return pushResponse(201), nil
	})}
	done := make(chan PhonePushResult, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- NotifyPhone(ctx, s.RuntimeDir, parallelNotice, client) }()
	}
	for i := 0; i < 8; i++ {
		awaitPushSignal(t, ctx, entered)
	}
	// Both callers have four active sends; the limit is not process-wide.
	if len(entered) != 0 {
		t.Fatal("呼出しあたり4件を超えて開始しました")
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case result := <-done:
			if result != (PhonePushResult{Sent: 8, Subscriptions: 8}) {
				t.Fatal(result)
			}
		case <-ctx.Done():
			t.Fatal("並行呼出しが終わりませんでした")
		}
	}
}

// A response status alone must not prune a subscription if receiving that
// response subsequently fails through cancellation or the client's timeout.
type canceledPushBody struct {
	ctx     context.Context
	reading chan<- struct{}
	closed  *atomic.Int32
}

func (b *canceledPushBody) Read([]byte) (int, error) {
	b.reading <- struct{}{}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *canceledPushBody) Close() error { b.closed.Add(1); return nil }

func TestPushExpiredResponseBodyCancellationKeepsSubscriptions(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelCaller), func(t *testing.T) {
			s, _ := persistedPushFixture(t, 4)
			guard := pushTestContext(t)
			ctx, cancel := context.WithCancel(guard)
			defer cancel()
			reading := make(chan struct{}, 4)
			var closed atomic.Int32
			timeout := time.Duration(0)
			if !cancelCaller {
				timeout = 2 * time.Second
			}
			s.HTTPClient = &http.Client{Timeout: timeout, Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				response := pushResponse(410)
				if pushIndex(r)%2 == 0 {
					response.StatusCode = 404
				}
				response.Body = &canceledPushBody{ctx: r.Context(), reading: reading, closed: &closed}
				return response, nil
			})}
			done := make(chan PhonePushResult, 1)
			go func() { done <- s.notifyPhone(ctx, parallelNotice) }()
			for i := 0; i < 4; i++ {
				awaitPushSignal(t, guard, reading)
			}
			if cancelCaller {
				cancel()
			}
			select {
			case result := <-done:
				if result != (PhonePushResult{Failed: 4, Subscriptions: 4}) {
					t.Fatal(result)
				}
			case <-guard.Done():
				t.Fatal("response bodyが回収されませんでした")
			}
			if closed.Load() != 4 || len(savedPushSubscriptions(t, s)) != 4 {
				t.Fatal("キャンセル・timeoutで購読を削除したか、bodyを閉じませんでした")
			}
		})
	}
}
