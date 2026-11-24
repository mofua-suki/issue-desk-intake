package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
)

func TestCredentialsSurviveRestartAndRevocation(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	s := openPostgresApplication(t, env)
	now := time.UnixMilli(1793490000123)
	token := PCTokenRecord{ID: "first-id", Name: "pc-one", Hash: auth.Hash("token-one"), IssuedAt: now}
	if added, err := s.AddPCToken(ctx, token); err != nil || !added {
		t.Fatalf("add: %v %v", added, err)
	}
	if added, err := s.AddPCToken(ctx, token); err != nil || added {
		t.Fatalf("duplicate: %v %v", added, err)
	}
	if changed, err := s.SetDelegated(ctx, token.ID, true, now); err != nil || !changed {
		t.Fatalf("delegate: %v %v", changed, err)
	}
	if err := s.TouchPCToken(ctx, token.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSession(ctx, auth.Hash("cookie-one"), auth.Session{CSRF: "csrf-one", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSetting(ctx, "future-setting", map[string]any{"version": 12, "nested": []any{"preserved", 42}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openPostgresApplication(t, env)
	got, found, err := s.PCToken(ctx, token.Hash)
	if err != nil || !found || got.ID != token.ID || !got.Delegated {
		t.Fatalf("restored token: %#v %v %v", got, found, err)
	}
	var used string
	if err := s.db.QueryRow("SELECT last_used_at FROM api_tokens WHERE id=?", token.ID).Scan(&used); err != nil || used != now.Add(time.Minute).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("last-used: %s %v", used, err)
	}
	session, found, err := s.Session(ctx, auth.Hash("cookie-one"))
	if err != nil || !found || session.CSRF != "csrf-one" || !session.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("restored session: %#v %v %v", session, found, err)
	}
	if removed, err := s.RevokePCToken(ctx, token.Name); err != nil || !removed {
		t.Fatalf("revoke: %v %v", removed, err)
	}
	if _, found, err := s.PCToken(ctx, token.Hash); err != nil || found {
		t.Fatalf("revoked token remained: %v %v", found, err)
	}
	token.ID, token.Hash = "second-id", auth.Hash("token-two")
	if _, err := s.AddPCToken(ctx, token); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.PCToken(ctx, token.Hash); err != nil || !found || got.Delegated {
		t.Fatalf("reissued token inherited grant: %#v %v %v", got, found, err)
	}
	if err := s.RevokeSession(ctx, auth.Hash("cookie-one")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openPostgresApplication(t, env)
	if _, found, err := s.Session(ctx, auth.Hash("cookie-one")); err != nil || found {
		t.Fatalf("revoked session remained: %v %v", found, err)
	}
	if _, found, err := s.Setting(ctx, "future-setting"); err != nil || !found {
		t.Fatalf("setting lost: %v %v", found, err)
	}
}

func TestOpeningExistingDataKeepsOtherDataIntact(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := env.DB.Exec(`INSERT INTO drafts(id,json) VALUES('draft-id','{"futureField":{"keep":true}}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.Exec(`INSERT INTO settings(id,json) VALUES('sessions','[["cookie-hash",{"csrf":"old-csrf","expires":1793493600123,"futureField":7}],["other-hash",{"csrf":"other","expires":1793493600123}]]')`); err != nil {
		t.Fatal(err)
	}
	s := openPostgresApplication(t, env)
	got, found, err := s.Session(ctx, "cookie-hash")
	if err != nil || !found || got.CSRF != "old-csrf" {
		t.Fatalf("existing session: %#v %v %v", got, found, err)
	}
	if err := s.PutSession(ctx, "cookie-hash", auth.Session{CSRF: "new-csrf", ExpiresAt: time.UnixMilli(1793497200123)}); err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.Setting(ctx, "sessions")
	if err != nil {
		t.Fatal(err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) != 2 {
		t.Fatalf("sessions overwritten: %s %v", raw, err)
	}
	var pairs [][2]json.RawMessage
	if err := json.Unmarshal(raw, &pairs); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(pairs[1][1], &fields); err != nil || fields["csrf"] != "other" {
		t.Fatalf("other session changed: %v %v", fields, err)
	}
	var draft string
	if err := s.db.QueryRow("SELECT json FROM drafts WHERE id='draft-id'").Scan(&draft); err != nil || draft != `{"futureField":{"keep":true}}` {
		t.Fatalf("draft changed: %s %v", draft, err)
	}
}

func TestCLIKeyConsumptionIsAtomicAcrossConnectionsAndExpiresInMilliseconds(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	first, second := openPostgresApplication(t, env), openPostgresApplication(t, env)
	now := time.UnixMilli(1793490000123)
	if err := first.AddCLIKey(ctx, "one-time", now); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := first
			if i%2 == 0 {
				s = second
			}
			used, err := s.ConsumeCLIKey(ctx, "one-time", now)
			if err != nil {
				t.Errorf("consume: %v", err)
			}
			if used {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("consumed %d times", accepted.Load())
	}
	if err := first.AddCLIKey(ctx, "expired", now.Add(-time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if used, err := second.ConsumeCLIKey(ctx, "expired", now); err != nil || used {
		t.Fatalf("expired key accepted: %v %v", used, err)
	}
	if err := first.AddCLIKey(ctx, "valid", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if used, err := second.ConsumeCLIKey(ctx, "valid", now); err != nil || !used {
		t.Fatalf("valid key rejected: %v %v", used, err)
	}
}

func TestInvalidSessionDataFailsWithoutOverwritingIt(t *testing.T) {
	s := openPostgresApplication(t, pgtest.NewIsolated(t))
	ctx := context.Background()
	if err := s.PutSetting(ctx, "sessions", "unexpected-string"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Session(ctx, "cookie"); err == nil {
		t.Fatal("invalid data was accepted")
	}
	if err := s.PutSession(ctx, "cookie", auth.Session{}); err == nil {
		t.Fatal("invalid data was overwritten")
	}
	raw, _, err := s.Setting(ctx, "sessions")
	if err != nil || string(raw) != `"unexpected-string"` {
		t.Fatalf("data changed: %s %v", raw, err)
	}
}

func checkApplicationDesktopConcurrent(t *testing.T, first, second *SQLite) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	now := time.UnixMilli(1793490000123)
	if err := first.PutSetting(ctx, "desktopToken", map[string]any{"hash": "parallel", "expires": now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s := first
			if i%2 == 0 {
				s = second
			}
			used, err := s.ConsumeDesktopKey(ctx, "parallel", now)
			if err != nil {
				t.Error("デスクトップキーの並行消費に失敗しました")
			}
			if used {
				successes.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("デスクトップキーの消費成功数: %d", successes.Load())
	}
	raw, ok, err := first.Setting(ctx, "desktopToken")
	if err != nil || !ok || string(raw) != "null" {
		t.Fatal("消費後の保存形式がJSON nullではありません")
	}
}

func TestDesktopConsumptionAcrossConnectionsAndRestart(t *testing.T) {
	env := pgtest.NewIsolated(t)
	first, second := openPostgresApplication(t, env), openPostgresApplication(t, env)
	checkApplicationDesktopConcurrent(t, first, second)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := openPostgresApplication(t, env)
	raw, ok, err := restarted.Setting(context.Background(), "desktopToken")
	if err != nil || !ok || string(raw) != "null" {
		t.Fatal("再起動後に消費結果が保持されていません")
	}
	used, err := restarted.ConsumeDesktopKey(context.Background(), "parallel", time.UnixMilli(1793490000123))
	if err != nil || used {
		t.Fatal("再起動後にキーを再消費できました")
	}
}

func TestApplicationReadDoesNotWaitForWriter(t *testing.T) {
	env := pgtest.NewIsolated(t)
	reader, writer := openPostgresApplication(t, env), openPostgresApplication(t, env)
	if err := reader.PutSetting(context.Background(), "saved", true); err != nil {
		t.Fatal(err)
	}
	tx, err := writer.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE settings SET json='false' WHERE id='saved'"); err != nil {
		t.Fatal(err)
	}
	// A reader must see the committed value without waiting for the open writer.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, found, err := reader.Setting(ctx, "saved")
	if err != nil || !found || string(raw) != "true" {
		t.Fatal("自動コミットの読取が書込の権利を待ちました")
	}
}

func TestApplicationCancellation(t *testing.T) {
	s := openPostgresApplication(t, pgtest.NewIsolated(t))
	if s.DB() != s.db.DB {
		t.Fatal("共有DBの契約が変わりました")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.PutSetting(ctx, "canceled", true); !errors.Is(err, context.Canceled) {
		t.Fatal("書込のcontextが伝わっていません")
	}
	if _, _, err := s.Setting(ctx, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatal("読取のcontextが伝わっていません")
	}
	if err := s.PutSession(ctx, "canceled", auth.Session{}); !errors.Is(err, context.Canceled) {
		t.Fatal("トランザクションのcontextが伝わっていません")
	}
	if _, found, err := s.Setting(context.Background(), "canceled"); err != nil || found {
		t.Fatal("キャンセルした書込が保存されました")
	}
}
