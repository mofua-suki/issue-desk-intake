package admincli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/startup"
	"issue-desk-intake/server/internal/statistics"
	"issue-desk-intake/server/internal/store"
)

func fixture(t *testing.T) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "config.local.json")
	raw, _ := json.Marshal(map[string]any{"workerId": "nas", "giteaUrl": "http://gitea:3000", "runtimeDir": "runtime", "tokenFile": "private/admin-token", "codex": "tools/codex", "repositories": []any{map[string]any{"name": "owner/app"}}})
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return []string{"--config", file, "--application-dir", dir}, filepath.Join(dir, "runtime", "state", "intake.sqlite")
}

func TestPCNameStorageAndLegacyRecording(t *testing.T) {
	online := usePostgresAppStore(t)
	records := pgFixture(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		valid bool
	}{
		{"", false}, {"a", true}, {strings.Repeat("Z", 100), true}, {strings.Repeat("a", 101), false},
		{"Az09._-", true}, {"a b", false}, {"a\nb", false}, {"a/b", false}, {"日本語", false},
	}
	for i, tc := range cases {
		var before, after int
		if err := online.DB().QueryRow("SELECT count(*) FROM api_tokens").Scan(&before); err != nil {
			t.Fatal(err)
		}
		added, err := online.AddPCToken(ctx, store.PCTokenRecord{ID: fmt.Sprintf("name-%d", i), Name: tc.name, Hash: hash(fmt.Sprintf("test-%d", i)), IssuedAt: time.Now()})
		if tc.valid && (err != nil || !added) || !tc.valid && (!errors.Is(err, store.ErrInvalidPCName) || added) {
			t.Fatalf("case %d: added=%v err=%v", i, added, err)
		}
		if err := online.DB().QueryRow("SELECT count(*) FROM api_tokens").Scan(&after); err != nil {
			t.Fatal(err)
		}
		want := before
		if tc.valid {
			want++
		}
		if after != want {
			t.Fatalf("case %d: count %d want %d", i, after, want)
		}
		if tc.valid {
			got, found, err := online.PCToken(ctx, hash(fmt.Sprintf("test-%d", i)))
			if err != nil || !found || got.Name != tc.name {
				t.Fatalf("case %d: read failed: %v", i, err)
			}
		}
	}
	// Existing invalid rows bypass the new insertion guard, as they did before it existed.
	legacyToken := "idk_" + strings.Repeat("c", 43)
	if _, err := online.DB().Exec("INSERT INTO api_tokens(id,name,hash,issued_at) VALUES(?,?,?,?)", "legacy", "PC 日本語", hash(legacyToken), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	validToken := "idk_" + strings.Repeat("v", 43)
	if added, err := online.AddPCToken(ctx, store.PCTokenRecord{ID: "valid-record", Name: "PC._-9", Hash: hash(validToken), IssuedAt: time.Now()}); err != nil || !added {
		t.Fatal("valid insert", err)
	}
	a, err := auth.New(auth.Config{Port: 8787}, &store.AuthStore{SQLite: online})
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct{ token, want string }{{validToken, "PC._-9"}, {legacyToken, "other"}} {
		r := httptest.NewRequest("POST", "http://localhost:8787/", nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		principal, problem := a.Authenticate(r)
		if problem != nil || principal.Kind != auth.PC {
			t.Fatalf("authentication failed: %v", problem)
		}
		if i == 0 && principal.Name != tc.want || i == 1 && principal.Name != "PC 日本語" {
			t.Fatal("authentication changed name")
		}
		id := fmt.Sprintf("123e4567-e89b-12d3-a456-%012d", i)
		err := statistics.WriteStopCheckRecords(ctx, records, []statistics.StopCheckRecord{{ID: id, CheckID: "unasked-send", Result: "block", OccurredAt: "2026-11-05T12:34:56Z", Project: "project", Host: principal.Name}})
		if err != nil {
			t.Fatal(err)
		}
		var host string
		if err := records.QueryRow("SELECT host FROM stop_check_records WHERE id=$1", id).Scan(&host); err != nil || host != tc.want {
			t.Fatalf("record host=%q err=%v", host, err)
		}
	}
}

func TestPCIssueNameValidation(t *testing.T) {
	online := usePostgresAppStore(t)
	args, _ := fixture(t)
	for _, tc := range []struct{ name, reason string }{
		{"", "入力"}, {"-abc", "先頭"}, {"Abc", "先頭"}, {"a_b", "使える文字"}, {"a b", "使える文字"}, {"a\nb", "使える文字"}, {"a/b", "使える文字"}, {"a日本語", "使える文字"}, {strings.Repeat("a", 32), "31文字"},
	} {
		var before, after int
		if err := online.DB().QueryRow("SELECT count(*) FROM api_tokens").Scan(&before); err != nil {
			t.Fatal(err)
		}
		out, notice, err := command(t, append(args, "pc-token", "issue", tc.name), "")
		if err == nil || !strings.Contains(err.Error(), tc.reason) || out != "" || notice != "" {
			t.Fatalf("invalid name: out=%q notice=%q err=%v", out, notice, err)
		}
		if err := online.DB().QueryRow("SELECT count(*) FROM api_tokens").Scan(&after); err != nil || after != before {
			t.Fatal("invalid name persisted", err)
		}
	}
	for _, name := range []string{"a", strings.Repeat("a", 31), "a-09"} {
		out, _, err := command(t, append(args, "pc-token", "issue", name), "")
		if err != nil || !strings.HasPrefix(out, "idk_") {
			t.Fatalf("valid name %q: %v", name, err)
		}
	}
}

// usePostgresAppStore runs the commands on an isolated application schema (the
// production tables) and returns a store on the same schema for the test to read.
func usePostgresAppStore(t *testing.T) *store.SQLite {
	t.Helper()
	previousDialect := appsql.SetActive(appsql.Postgres)
	t.Cleanup(func() { appsql.SetActive(previousDialect) })
	env := pgtest.NewIsolated(t)
	open := func(ctx context.Context) (*store.SQLite, error) {
		return store.OpenApplicationPostgres(ctx, env.DSN, env.Schema)
	}
	previous := openAppStore
	openAppStore = func(ctx context.Context, c *startup.Config) (*store.SQLite, error) { return open(ctx) }
	t.Cleanup(func() { openAppStore = previous })
	db, err := open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func command(t *testing.T, args []string, input string) (string, string, error) {
	t.Helper()
	var out, notice bytes.Buffer
	err := Run(context.Background(), args, strings.NewReader(input), &out, &notice)
	return out.String(), notice.String(), err
}
func TestPCTokenLifecycleUsesOnlineStoreWithoutDelegationOrSecretListing(t *testing.T) {
	online := usePostgresAppStore(t)
	args, _ := fixture(t)
	ctx := context.Background()
	issued, notice, err := command(t, append(args, "pc-token", "issue", "test-host"), "")
	token := strings.TrimSpace(issued)
	if err != nil || !regexp.MustCompile(`^idk_[A-Za-z0-9_-]{43}\n$`).MatchString(issued) || strings.Contains(notice, token) {
		t.Fatalf("発行に失敗しました: %v", err)
	}
	got, found, err := online.PCToken(ctx, hash(token))
	if err != nil || !found || got.Delegated || got.Name != "test-host" {
		t.Fatal("オンライン受付から新規tokenを照合できません", err)
	}
	duplicate, duplicateNotice, duplicateErr := command(t, append(args, "pc-token", "issue", "test-host"), "")
	if duplicateErr == nil || duplicate != "" || strings.Contains(duplicateNotice+duplicateErr.Error(), token) || strings.Contains(duplicateNotice+duplicateErr.Error(), hash(token)) {
		t.Fatal("同名を再発行しました")
	}
	listed, notice, err := command(t, append(args, "pc-token", "list"), "")
	if err != nil || !strings.Contains(listed, "test-host") || strings.Contains(listed, token) || strings.Contains(listed, hash(token)) || strings.Contains(notice, token) {
		t.Fatal("一覧が秘密を含むか名前を失っています")
	}
	if out, _, err := command(t, append(args, "pc-token", "revoke", "test-host"), ""); err != nil || out != "" {
		t.Fatal("失効できません", err)
	}
	if _, found, err = online.PCToken(ctx, hash(token)); err != nil || found {
		t.Fatal("オンライン受付で失効しません", err)
	}
}

func TestPCIssueStorageFailureDoesNotExposeSecrets(t *testing.T) {
	online := usePostgresAppStore(t)
	args, _ := fixture(t)
	const secret = "private-db-credential-marker"
	t.Setenv("ISSUE_DESK_DATABASE_URL", "postgres://user:"+secret+"@localhost/issue_desk_test")
	if err := online.Close(); err != nil {
		t.Fatal(err)
	}
	previous := openAppStore
	openAppStore = func(context.Context, *startup.Config) (*store.SQLite, error) { return online, nil }
	t.Cleanup(func() { openAppStore = previous })
	out, notice, err := command(t, append(args, "pc-token", "issue", "test-host"), "")
	if err == nil || err.Error() != "PCトークンを保存できません" || out != "" || notice != "" || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "idk_") {
		t.Fatal("保存失敗の出力が不正です")
	}
}
func pgFixture(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("専用PostgreSQL未設定")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("専用DBのみ使用できます")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid()
	if err != nil {
		t.Fatal(err)
	}
	schema := "admincli_" + strings.ReplaceAll(id, "-", "")
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	t.Setenv("ISSUE_DESK_DATABASE_URL", u.String())
	db, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	if _, err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}
func TestPostgresHostdTokenLifecycleAndClientReleaseRegistration(t *testing.T) {
	db := pgFixture(t)
	args, _ := fixture(t)
	out, notice, err := command(t, append(args, "hostd-token", "issue", "test-host"), "")
	token := strings.TrimSpace(out)
	if err != nil || !regexp.MustCompile(`^idh_[A-Za-z0-9_-]{43}\n$`).MatchString(out) || strings.Contains(notice, token) {
		t.Fatal("hostdを発行できません", err)
	}
	var saved string
	if err = db.QueryRow("SELECT hash FROM host_image_tokens WHERE host='test-host'").Scan(&saved); err != nil || saved != hash(token) {
		t.Fatal("hostdのハッシュを保存できません", err)
	}
	if out, _, err := command(t, append(args, "hostd-token", "issue", "test-host"), ""); err == nil || out != "" {
		t.Fatal("hostdを重複発行しました")
	}
	if out, _, err := command(t, append(args, "hostd-token", "revoke", "test-host"), ""); err != nil || out != "" {
		t.Fatal("hostdを失効できません", err)
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM host_image_tokens").Scan(&n); err != nil || n != 0 {
		t.Fatal("hostd失効がDBへ反映されません", err)
	}
	body := `{"host":"test-host","registry":"registry.example:5000","revision":"` + strings.Repeat("a", 40) + `","tag":"` + strings.Repeat("a", 12) + `","digest":"sha256:` + strings.Repeat("b", 64) + `"}`
	if out, _, err := command(t, append(args, "client-release", "register"), body); err != nil || out != "" {
		t.Fatal("版を登録できません", err)
	}
	var rev string
	if err = db.QueryRow("SELECT revision FROM releases WHERE host='test-host'").Scan(&rev); err != nil || rev != strings.Repeat("a", 40) {
		t.Fatal("版がDBへ反映されません", err)
	}
	if _, err = db.Exec("INSERT INTO releases(host,flavor,registry,tag,digest,revision,kind) SELECT '*',flavor,registry,tag,digest,revision,kind FROM releases WHERE host='test-host'"); err != nil {
		t.Fatal(err)
	}
	previous, _, err := command(t, append(args, "client-release", "previous"), "")
	if err != nil || strings.TrimSpace(previous) != strings.Repeat("a", 40) {
		t.Fatal("導入が使う希望版の読取に失敗しました", err)
	}
}
func TestRejectedInputAndConnectionNeverEchoSecrets(t *testing.T) {
	args, _ := fixture(t)
	t.Setenv("ISSUE_DESK_DATABASE_URL", "postgres://private-user:private-secret@127.0.0.1:1/private-db?connect_timeout=1")
	out, notice, err := command(t, append(args, "hostd-token", "issue", "test-host"), "")
	if err == nil || strings.Contains(out+notice+err.Error(), "private-secret") {
		t.Fatal("接続の秘密が露出しました")
	}
	for _, key := range []string{"ISSUE_DESK_DATABASE_URL", "PGHOST", "PGUSER", "PGDATABASE"} {
		t.Setenv(key, "")
	}
	if _, _, err := command(t, append(args, "hostd-token", "issue", "test-host"), ""); err == nil {
		t.Fatal("接続設定なしで操作を開始しました")
	}
}

func TestCLIKeyUsesOneMinuteAndSingleConsumptionWithoutDelegation(t *testing.T) {
	db := usePostgresAppStore(t)
	args, _ := fixture(t)
	ctx := context.Background()
	output, notice, err := command(t, append(args, "cli-key", "issue"), "")
	key := strings.TrimSpace(output)
	if err != nil || !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(key) || notice != "" {
		t.Fatal("一時キーを準備できません", err)
	}
	var expires int64
	if err = db.DB().QueryRow("SELECT expires FROM cli_keys WHERE hash=?", hash(key)).Scan(&expires); err != nil || expires < time.Now().UnixMilli()+55000 || expires > time.Now().UnixMilli()+60000 {
		t.Fatal("一時キーのTTLが1分ではありません", err)
	}
	found, err := db.ConsumeCLIKey(ctx, hash(key), time.Now())
	if err != nil || !found {
		t.Fatal("一時キーを消費できません", err)
	}
	found, err = db.ConsumeCLIKey(ctx, hash(key), time.Now())
	if err != nil || found {
		t.Fatal("一時キーを再利用しました", err)
	}
	output, _, err = command(t, append(args, "cli-key", "issue"), "")
	if err != nil {
		t.Fatal(err)
	}
	if out, _, err := command(t, append(args, "cli-key", "revoke"), output); err != nil || out != "" {
		t.Fatal("未消費キーを回収できません", err)
	}
	var count int
	if err = db.DB().QueryRow("SELECT count(*) FROM cli_keys").Scan(&count); err != nil || count != 0 {
		t.Fatal("自分のキーが残りました", err)
	}
}
