package adminapi

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func pushInput(t testing.TB, endpoint string) map[string]any {
	t.Helper()
	key, e := ecdh.P256().GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	secret := make([]byte, 16)
	if _, e = rand.Read(secret); e != nil {
		t.Fatal(e)
	}
	return map[string]any{"endpoint": endpoint, "keys": map[string]any{"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(secret)}}
}
func TestPushPersistenceEncryptionAndEndpointLimits(t *testing.T) {
	f := newFixture(t)
	code, value := f.call("GET", "/api/push/key", "screen", nil)
	status(t, code, 200, value)
	public := obj(value)["publicKey"]
	if len(str(public)) != 87 {
		t.Fatal("公開鍵が不正です")
	}
	input := pushInput(t, "https://web.push.apple.com/test-push")
	code, value = f.call("POST", "/api/push/subscriptions", "screen", input)
	status(t, code, 200, value)
	f.restart()
	code, value = f.call("GET", "/api/push/key", "screen", nil)
	status(t, code, 200, value)
	if obj(value)["publicKey"] != public {
		t.Fatal("再起動で鍵が変わりました")
	}
	calls := 0
	f.options.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "web.push.apple.com" || r.Header.Get("TTL") != "300" || r.Header.Get("Authorization") == "" || r.Header.Get("Content-Encoding") != "aes128gcm" {
			t.Error("WebPush署名と暗号化のヘッダーが不足しています")
			return nil, errors.New("invalid Web Push headers")
		}
		raw, e := io.ReadAll(r.Body)
		if e != nil || strings.Contains(string(raw), "テスト通知") {
			t.Error("通知が暗号化されていません")
			return nil, errors.New("invalid encrypted payload")
		}
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	f.restart()
	code, value = f.call("POST", "/api/push/test", "screen", map[string]any{})
	status(t, code, 200, value)
	if calls != 1 {
		t.Fatalf("送信=%d", calls)
	}
	for _, endpoint := range []string{"http://web.push.apple.com/a", "https://127.0.0.1/a", "https://web.push.apple.com:444/a", "https://user@web.push.apple.com/a", "https://web.push.apple.com/a#fragment", "https://web.push.apple.com.evil.invalid/a"} {
		input["endpoint"] = endpoint
		code, value = f.call("POST", "/api/push/subscriptions", "screen", input)
		status(t, code, 400, value)
	}
	file := filepath.Join(f.directory, "state", "web-push.json")
	info, e := os.Stat(file)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("保存権限: %v", e)
	}
	raw, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	var state map[string]any
	if e = json.Unmarshal(raw, &state); e != nil || len(state["subscriptions"].([]any)) != 1 {
		t.Fatalf("状態: %v", e)
	}
}
func TestPushGoneKeepsConcurrentReplacementAndRejectsRedirect(t *testing.T) {
	f := newFixture(t)
	endpoint := "https://fcm.googleapis.com/test-push"
	original := pushInput(t, endpoint)
	code, value := f.call("POST", "/api/push/subscriptions", "screen", original)
	status(t, code, 200, value)
	replacement := pushInput(t, endpoint)
	sender := &service{Options: Options{RuntimeDir: f.directory, Now: func() time.Time { return f.now }}}
	f.options.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		e := sender.withPush(func(state map[string]any) (bool, error) {
			state["subscriptions"] = []any{replacement}
			return true, nil
		})
		if e != nil {
			t.Error(e)
			return nil, e
		}
		return &http.Response{StatusCode: 410, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	result := NotifyPhone(context.Background(), f.directory, PhoneNotice{Title: "試験", Body: "本文", URL: "/"}, f.options.HTTPClient)
	if result.Failed != 1 {
		t.Fatal(result)
	}
	e := sender.withPush(func(state map[string]any) (bool, error) {
		list := state["subscriptions"].([]any)
		if len(list) != 1 || obj(obj(list[0])["keys"])["auth"] != obj(replacement["keys"])["auth"] {
			t.Fatal("送信中に更新した購読を削除しました")
		}
		return false, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	calls := 0
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://127.0.0.1/private"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	result = NotifyPhone(context.Background(), f.directory, PhoneNotice{Title: "試験", Body: "本文", URL: "/"}, client)
	if calls != 1 || result.Failed != 1 {
		t.Fatalf("redirect calls=%d result=%+v", calls, result)
	}
}
