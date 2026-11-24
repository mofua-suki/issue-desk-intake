package adminapi

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/hkdf"
)

// Decode the actual RFC8291 wire payload at the fake receiving endpoint.
func decryptPushNotice(t *testing.T, r *http.Request, receiver *ecdh.PrivateKey, auth []byte) PhoneNotice {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil || len(raw) < 86 || raw[20] != 65 {
		t.Error("invalid encrypted record")
		return PhoneNotice{}
	}
	sender, err := ecdh.P256().NewPublicKey(raw[21:86])
	if err != nil {
		t.Error(err)
		return PhoneNotice{}
	}
	shared, err := receiver.ECDH(sender)
	if err != nil {
		t.Error(err)
		return PhoneNotice{}
	}
	derive := func(secret, salt, info []byte, size int) []byte {
		output := make([]byte, size)
		if _, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, info), output); err != nil {
			t.Error(err)
		}
		return output
	}
	info := append([]byte("WebPush: info\x00"), receiver.PublicKey().Bytes()...)
	info = append(info, sender.Bytes()...)
	ikm := derive(shared, auth, info, 32)
	key := derive(ikm, raw[:16], []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := derive(ikm, raw[:16], []byte("Content-Encoding: nonce\x00"), 12)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Error(err)
		return PhoneNotice{}
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Error(err)
		return PhoneNotice{}
	}
	plain, err := gcm.Open(nil, nonce, raw[86:], nil)
	if err != nil {
		t.Error(err)
		return PhoneNotice{}
	}
	plain = []byte(strings.TrimRight(string(plain), "\x00"))
	if len(plain) == 0 || plain[len(plain)-1] != 2 {
		t.Error("invalid encrypted delimiter")
		return PhoneNotice{}
	}
	var notice PhoneNotice
	if err := json.Unmarshal(plain[:len(plain)-1], &notice); err != nil {
		t.Error(err)
	}
	return notice
}

func TestPushParallelAPIContractAndAccess(t *testing.T) {
	f := newFixture(t)
	receiver, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err = rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	// Register the actual maximum through the API, preserving its stored format.
	for i := 0; i < 32; i++ {
		input := map[string]any{
			"endpoint": "https://fcm.googleapis.com/" + strconv.Itoa(i),
			"keys":     map[string]any{"p256dh": base64.RawURLEncoding.EncodeToString(receiver.PublicKey().Bytes()), "auth": base64.RawURLEncoding.EncodeToString(auth)},
		}
		code, value := f.call("POST", "/api/push/subscriptions", "screen", input)
		status(t, code, 200, value)
	}
	code, value := f.call("POST", "/api/push/subscriptions", "screen", pushInput(t, "https://fcm.googleapis.com/extra"))
	status(t, code, 400, value)

	code, value = f.call("GET", "/api/push/key", "screen", nil)
	status(t, code, 200, value)
	public := str(obj(value)["publicKey"])
	publicBytes, err := base64.RawURLEncoding.DecodeString(public)
	if err != nil {
		t.Fatal(err)
	}
	vapidKey, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), publicBytes)
	if err != nil {
		t.Fatalf("invalid VAPID public key: %v", err)
	}
	var calls atomic.Int32
	f.options.HTTPClient = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "" || r.Header.Get("TTL") != "300" || r.Header.Get("Content-Encoding") != "aes128gcm" {
			t.Error("Web Push headers missing")
		}

		token, headerKey, ok := strings.Cut(strings.TrimPrefix(r.Header.Get("Authorization"), "vapid t="), ", k=")
		if !ok || headerKey != public {
			t.Error("VAPID key changed")
		}
		if _, err := jwt.Parse(token, func(*jwt.Token) (any, error) { return vapidKey, nil }, jwt.WithValidMethods([]string{"ES256"}), jwt.WithAudience("https://fcm.googleapis.com")); err != nil {
			t.Error("invalid VAPID signature or audience")
		}
		notice := decryptPushNotice(t, r, receiver, auth)
		if notice != (PhoneNotice{Title: "開発受付", Body: "テスト通知です。", URL: "/"}) {
			t.Error("fixed notification changed")
		}
		return pushResponse(503), nil
	})}
	f.restart() // Reload the shipped persisted state and serve/authentication stack.
	for _, denied := range []struct {
		credential string
		headers    map[string]string
		want       int
	}{
		{"", map[string]string{"Origin": "http://127.0.0.1:8787"}, 401}, {"", nil, 403}, {"worker", nil, 403}, {"cli", nil, 403},
		{"screen", map[string]string{"Origin": "https://other.invalid"}, 403},
		{"screen", map[string]string{"Origin": ""}, 403},
		{"screen", map[string]string{"X-CSRF-Token": "wrong"}, 403},
		{"screen", map[string]string{"X-CSRF-Token": ""}, 403},
		{"screen", map[string]string{"X-Desk-Key": ""}, 403},
		{"screen", map[string]string{"Authorization": "Bearer " + f.token["worker"]}, 403},
	} {
		code, value = f.call("POST", "/api/push/test", denied.credential, map[string]any{}, denied.headers)
		status(t, code, denied.want, value)
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized delivery")
	}
	code, value = f.call("POST", "/api/push/test", "screen", map[string]any{"body": "上書きしない"})
	status(t, code, 200, value)
	if obj(value)["ok"] != true || calls.Load() != 32 {
		t.Fatalf("ok=%v calls=%d", obj(value)["ok"], calls.Load())
	}
	sender := &service{Options: Options{RuntimeDir: f.directory}}
	if len(savedPushSubscriptions(t, sender)) != 32 {
		t.Fatal("transient failures removed subscriptions")
	}
	for _, name := range []string{"web-push.json", "web-push.json.lock"} {
		info, err := os.Stat(filepath.Join(f.directory, "state", name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("state permissions: %v", err)
		}
	}
	// The exported entry point used by normal notifications shares result semantics.
	result := NotifyPhone(context.Background(), f.directory, parallelNotice, &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) { return pushResponse(201), nil })})
	if result != (PhonePushResult{Sent: 32, Subscriptions: 32}) {
		t.Fatal(result)
	}
}
