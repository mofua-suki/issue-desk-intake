package contract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

type wrappedCause struct{ cause error }

func (e *wrappedCause) Error() string { return "外部サービスへ接続できません。" }
func (e *wrappedCause) Unwrap() error { return e.cause }

func TestSafeCauseKeepsWrappedCauseAndRemovesCredentials(t *testing.T) {
	secrets := []string{"idk_abcdefghijklmnop", "fixtureBearerValue1234", "fixturePassword", "ghp_" + "abcdefghijklmnopqrst", "fixtureHeaderValue"}
	err := &wrappedCause{cause: fmt.Errorf("dial tcp 10.0.0.1:443: connection refused token=%s Authorization: Bearer %s\npassword: %s https://user:pw@example.test %s Cookie: session=%s", secrets[0], secrets[1], secrets[2], secrets[3], secrets[4])}
	got := SafeCause(err)
	for _, want := range []string{"外部サービスへ接続できません。", "connection refused"} {
		if !strings.Contains(got, want) {
			t.Fatalf("元の原因が消えています: %q", got)
		}
	}
	for _, secret := range append(secrets, "user:pw@") {
		if strings.Contains(got, secret) {
			t.Fatalf("資格情報が残っています: %q", got)
		}
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("1行になっていません: %q", got)
	}
}

func TestSafeCauseIsBounded(t *testing.T) {
	if got := SafeCause(errors.New(strings.Repeat("あ", 2000))); UTF16Length(got) > causeLimit+1 {
		t.Fatalf("長さの上限を超えています: %d", UTF16Length(got))
	}
	if SafeCause(nil) != "" {
		t.Fatal("nil は空にします")
	}
}

func TestLogFailureWritesMaskedCauseOnce(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, nil)))
	defer slog.SetDefault(previous)
	LogFailure("経路", errors.New("disk full token=fixture-secret-value"))
	LogFailure("経路", nil)
	LogFailure("経路", context.Canceled)
	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "disk full") || strings.Contains(got, "fixture-secret-value") {
		t.Fatalf("ログの内容が違います: %q", got)
	}
}
