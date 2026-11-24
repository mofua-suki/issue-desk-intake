package startup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/httpapi"
	"issue-desk-intake/server/internal/store"
)

// appPostgres returns the migrated PostgreSQL schema (outbox, jobs and so on) that
// OpenApp receives as its database. The application store has a schema of its own
// (useApplicationSchema).
func appPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("隔離した検証用PostgreSQLのDSNが未設定です")
	}
	u, e := url.Parse(dsn)
	if e != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("明示的な検証DBが必要です")
	}
	admin, e := store.OpenPostgres(context.Background(), dsn)
	if e != nil {
		t.Fatal(e)
	}
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	schema := "startup_" + hex.EncodeToString(nonce)
	if _, e = admin.Exec("CREATE SCHEMA " + schema); e != nil {
		t.Fatal(e)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, e := store.OpenPostgres(context.Background(), u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		db.Close()
		if _, e := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); e != nil {
			t.Error(e)
		}
		admin.Close()
	})
	if _, e = store.Migrate(context.Background(), db); e != nil {
		t.Fatal(e)
	}
	return db
}
func TestOpenAppRegistersBusinessAPIsAndPreservesStatePostgres(t *testing.T) {
	pg := appPostgres(t)
	ctx := context.Background()
	dir := t.TempDir()
	var writes atomic.Int32
	var urgentReads atomic.Int32
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token fixture-intake-token" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" {
			writes.Add(1)
			w.WriteHeader(500)
			return
		}
		switch r.URL.Path {
		case "/api/v1/user":
			json.NewEncoder(w).Encode(map[string]any{"login": "intake", "id": 1})
		case "/api/v1/users/owner":
			json.NewEncoder(w).Encode(map[string]any{"login": "owner", "id": 2})
		case "/api/v1/repos/owner/repo/issues/999":
			urgentReads.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"state": "closed"})
		case "/api/v1/repos/owner/repo/issues":
			json.NewEncoder(w).Encode([]any{})
		default:
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"message": "fixture route unavailable"})
		}
	}))
	defer git.Close()
	private := filepath.Join(dir, "private")
	if e := os.MkdirAll(private, 0700); e != nil {
		t.Fatal(e)
	}
	tokenFile := filepath.Join(private, "intake-token")
	if e := os.WriteFile(tokenFile, []byte("fixture-intake-token"), 0600); e != nil {
		t.Fatal(e)
	}
	adminToken := filepath.Join(private, "admin-token")
	if e := os.WriteFile(adminToken, []byte("fixture-admin-token"), 0600); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(dir, "config.local.json")
	values := map[string]any{"workerId": "nas", "giteaUrl": git.URL, "runtimeDir": "runtime", "tokenFile": adminToken, "intakeTokenFile": tokenFile, "intakeAccount": "intake", "gitUser": "owner", "codex": "/missing-fixture-codex", "repositories": []any{map[string]any{"name": "owner/repo"}}, "intake": map[string]any{"publicUrl": "http://localhost:8787", "port": 8787, "aiConcurrency": 1}}
	writeConfig(t, file, values)
	writeConfig(t, filepath.Join(dir, "fleet.json"), map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"nas": map[string]any{"giteaUrl": git.URL, "repositories": []any{map[string]any{"name": "owner/repo"}}}}})
	writeConfig(t, filepath.Join(dir, "assignments.json"), map[string]any{"pollSeconds": 300, "hosts": map[string]any{"nas": map[string]any{"maxImplementations": 1, "repositories": map[string]any{"owner/repo": map[string]any{"rank": 1}}}}, "repositories": map[string]any{}})
	config, e := Load(file, dir, "")
	if e != nil {
		t.Fatal(e)
	}
	sqlite := openAppStore(t, useApplicationSchema(t, config))
	records, e := consultations.OpenRecords(ctx, sqlite.DB())
	if e != nil {
		t.Fatal(e)
	}
	todoID := "11111111-1111-4111-8111-111111111111"
	_, e = records.Change(ctx, "todos", todoID, func(*sql.Tx, consultations.Record) (consultations.Record, error) {
		return consultations.Record{"id": todoID, "project": "owner/repo", "text": "保存済みのTODO", "createdAt": "2026-10-01T00:00:00.000Z", "extra": map[string]any{"unknown": "keep"}}, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	// 起票前確認の判定が上限に達した相談は、起動の経路を通して止まりの一覧（要対応）へ出る。
	limitDraftID := "22222222-2222-4222-8222-222222222222"
	_, e = records.Change(ctx, "drafts", limitDraftID, func(*sql.Tx, consultations.Record) (consultations.Record, error) {
		return consultations.Record{"id": limitDraftID, "project": "owner/repo", "status": "draft", "revision": 1, "title": "上限に達した相談", "createdAt": "2026-10-01T00:00:00.000Z", "updatedAt": "2026-10-01T00:00:00.000Z", "messages": []any{}, "questions": []any{},
			"filingCheckCalls": map[string]any{"revision": 1, "count": 5, "outcome": "refused", "checks": []any{map[string]any{"at": "2026-10-01T00:05:00.000Z", "result": "refused", "codeRevision": strings.Repeat("c", 40)}}, "budget": map[string]any{"evidence": 0, "decisions": 5, "pending": false}}}, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	if e = sqlite.CreateSession(ctx, auth.Hash("fixture-session"), auth.Session{CSRF: "fixture-csrf", ExpiresAt: now.Add(time.Hour)}, now); e != nil {
		t.Fatal(e)
	}
	pc := "idk_" + strings.Repeat("a", 43)
	hostd := "idh_" + strings.Repeat("b", 43)
	if _, e = sqlite.AddPCToken(ctx, store.PCTokenRecord{ID: "fixture-worker", Name: "nas", Hash: auth.Hash(pc), IssuedAt: now}); e != nil {
		t.Fatal(e)
	}
	// 運用の代行を許したPCトークン（desk-alerts が要対応を読む経路）。
	delegatePC := "idk_" + strings.Repeat("f", 43)
	if _, e = sqlite.AddPCToken(ctx, store.PCTokenRecord{ID: "fixture-delegate", Name: "delegate-pc", Hash: auth.Hash(delegatePC), IssuedAt: now}); e != nil {
		t.Fatal(e)
	}
	if ok, e := sqlite.SetDelegated(ctx, "fixture-delegate", true, now); e != nil || !ok {
		t.Fatal("代行の許可を登録できません", ok, e)
	}
	if _, e = pg.Exec(`INSERT INTO host_image_tokens(host,hash)VALUES('nas',$1)`, auth.Hash(hostd)); e != nil {
		t.Fatal(e)
	}
	stageApplicationAssets(t, dir)
	// The start reads the ledger at the configured path and restores its numbers.
	if e = os.MkdirAll(filepath.Dir(config.LedgerFile), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(config.LedgerFile, []byte("{\"number\":42,\"project\":\"owner/ledger-recovery\",\"gitea\":7,\"at\":\"then\"}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	app, e := OpenApp(ctx, config, pg, strings.Repeat("c", 40), func(string) {})
	if e != nil {
		t.Fatal(e)
	}
	defer app.Close()
	if number, found, err := app.Numbers.Number(ctx, "owner/ledger-recovery", 7); err != nil || !found || number != 42 {
		t.Fatalf("起動時の台帳復旧: number=%d found=%v err=%v", number, found, err)
	}
	savedExecute := app.Runner.Execute
	app.Runner.Execute = func(context.Context, ai.ProcessRequest) (ai.ProcessResult, error) {
		return ai.ProcessResult{Code: 1}, nil
	}
	providerRows, providerErr := app.workerOptions().ModelProviders(ctx)
	app.Runner.Execute = savedExecute
	if providerErr != nil || len(providerRows) != 2 || providerRows[0]["id"] != "codex" || providerRows[1]["id"] != "claude" {
		t.Fatalf("無効化APIの実行先識別子が起動時に接続されていません: %v %v", providerRows, providerErr)
	}
	if app.SizeReference == nil || app.MeasurementReference == nil || app.Runner.ObserveResult == nil || app.RuntimeMeasurements == nil {
		t.Fatal("監視または参照資料の実体接続なし")
	}
	if text, e := app.SizeReference(ctx, "owner/repo"); e != nil || !strings.Contains(text, "マージされた依頼なし") {
		t.Fatal(text, e)
	}
	if text, e := app.MeasurementReference(ctx, "owner/repo"); e != nil || !strings.Contains(text, "DB使用量: 取得成功") {
		t.Fatal(text, e)
	}
	app.Runner.ObserveResult(ai.Observation{Step: "受付の相談", Provider: "codex", Model: "fixture", StartedAt: time.Now().Add(-time.Second), FinishedAt: time.Now(), Outcome: "failed", Cause: "input_invalid"})
	source, e := app.Operations()
	if e != nil {
		t.Fatal(e)
	}
	for _, link := range []func(string, string, int64) string{source.PublicLink, app.workerOptions().PublicLink} {
		if link == nil || link("owner/repo", "issues", 7) != git.URL+"/owner/repo/issues/7" {
			t.Fatal("画面とHTTPの公開参照が同じ設定から結線されていません")
		}
	}
	// Background workers can independently read the same issue; isolate the
	// two requests whose shared cache this assertion measures.
	app.StopBackground()
	// The production factory returns a fresh operations service on every request.
	if _, e = pg.Exec(`INSERT INTO jobs(repo,issue,title,status,priority,updated_at) VALUES('owner/repo',999,'cache-check','failed',1,$1)`, time.Now().Add(-time.Hour)); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		requestOps, err := app.Operations()
		if err != nil {
			t.Fatal(err)
		}
		requestOps.UrgentLookupTimeout = time.Hour
		requestOps.UrgentCacheTTL = time.Hour
		alerts, err := requestOps.Stalls(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range alerts {
			row, _ := raw.(map[string]any)
			if strings.Contains(fmt.Sprint(row["detail"]), "owner/repo#999 ") {
				t.Fatal("closed issue displayed", row)
			}
		}
	}
	if urgentReads.Load() != 1 {
		t.Fatalf("issue reads across requests = %d", urgentReads.Load())
	}
	if _, e = pg.Exec(`DELETE FROM jobs WHERE repo='owner/repo' AND issue=999`); e != nil {
		t.Fatal(e)
	}
	if alerts, e := source.AIAlerts(ctx); e != nil || len(alerts) == 0 {
		t.Fatal("AI観測が運用へ届かない", alerts, e)
	}
	assets := filepath.Join(dir, "assets")
	ui := strings.Repeat("d", 64)
	if e = os.MkdirAll(filepath.Join(assets, "v", ui), 0700); e != nil {
		t.Fatal(e)
	}
	for name, body := range map[string]string{"index.html": "<!doctype html><title>fixture</title>", "sw.js": "// fixture", "ui-version.json": `{"ui":"` + ui + `"}`, "v/" + ui + "/app.js": "// fixture"} {
		if e = os.WriteFile(filepath.Join(assets, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	server, e := httpapi.New(httpapi.Options{API: app.Mux, Assets: assets})
	if e != nil {
		t.Fatal(e)
	}
	defer server.Close()
	call := func(method, path, actor, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "http://localhost:8787"+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(contract.Header, contract.Version)
		switch actor {
		case "screen":
			request.AddCookie(&http.Cookie{Name: "desk_session", Value: "fixture-session"})
			request.Header.Set("Origin", "http://localhost:8787")
			request.Header.Set("X-CSRF-Token", "fixture-csrf")
		case "delegate":
			request.Header.Set("Authorization", "Bearer "+delegatePC)
		case "pc":
			request.Header.Set("Authorization", "Bearer "+pc)
		case "hostd":
			request.Header.Set("Authorization", "Bearer "+hostd)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	checkHostRepositoriesListHTTP(t, dir, pg, call)
	checkUsageReviewListHTTP(t, sqlite.DB(), pg, call)
	spec := call("GET", "/api/openapi.json", "pc", "")
	shipped, e := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi.json"))
	if e != nil || spec.Code != 200 || spec.Body.String() != string(shipped) {
		t.Fatal("出荷するAPI仕様を実際の認証付き入口から配信できません", spec.Code, e)
	}
	for _, item := range []struct {
		method, path, actor, body string
		status                    int
	}{
		{"GET", "/", "", "", 200}, {"GET", "/api/session", "screen", "", 200}, {"GET", "/api/todos", "screen", "", 200}, {"GET", "/api/list", "screen", "", 200},
		{"GET", "/api/proposals/dismissals", "screen", "", 404}, {"POST", "/api/proposals/restore", "screen", "{}", 404},
		{"GET", "/api/list", "pc", "", 403}, {"GET", "/api/todos", "", "", 401}, {"GET", "/api/proposals/dismissal", "pc", "", 403},
		{"GET", "/api/v1/client-config", "pc", "", 200}, {"GET", "/api/v1/client-config", "hostd", "", 403},
		{"GET", "/api/v1/hosts/nas/desired-image", "hostd", "", 200}, {"GET", "/api/v1/hosts/nas/desired-image", "pc", "", 403},
	} {
		response := call(item.method, item.path, item.actor, item.body)
		if response.Code != item.status {
			t.Errorf("%s %s %s: %d %s", item.method, item.path, item.actor, response.Code, response.Body.String())
		}
	}
	response := call("GET", "/api/meta", "screen", "")
	var meta map[string]any
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &meta) != nil || meta["runtimeMeasurements"] == nil {
		t.Fatal("Go実測のmeta結線", response.Code, response.Body.String())
	}
	response = call("GET", "/api/stalls/alerts", "screen", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "構造化出力のスキーマが不正です") {
		t.Fatal("画面のAI警告結線", response.Code, response.Body.String())
	}
	var stallRows []map[string]any
	if e = json.Unmarshal(response.Body.Bytes(), &stallRows); e != nil {
		t.Fatal(e)
	}
	var limitAlert map[string]any
	for _, row := range stallRows {
		if row["id"] == "filing-check-limit-"+limitDraftID {
			limitAlert = row
		}
	}
	if limitAlert == nil || limitAlert["kind"] != "intake_error" || limitAlert["status"] != "filing_check_stopped" || limitAlert["codeRevisionChanged"] != false || limitAlert["currentCodeRevision"] != nil || limitAlert["draftId"] != limitDraftID || limitAlert["requiresUser"] != true || limitAlert["nextAction"] == "" || !strings.Contains(fmt.Sprint(limitAlert["detail"]), "判定5回/上限5回") || limitAlert["detectedAt"] != "2026-10-01T00:05:00.000Z" {
		t.Fatalf("起票前確認の上限通知が止まりの一覧に結線されていません: %v", limitAlert)
	}
	if stalls, e := source.Stalls(ctx); e != nil || !strings.Contains(fmt.Sprint(stalls), "filing-check-limit-"+limitDraftID) {
		t.Fatal("画面の止まりの一覧に上限通知が出ません", e)
	}
	response = call("GET", "/api/stalls/alerts", "delegate", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "filing-check-limit-"+limitDraftID) {
		t.Fatal("代行を許したPCトークンの止まりの一覧に上限通知が出ません", response.Code)
	}
	// 画面の非表示（後片付けの照合と記録）を通すと、上限通知は一覧から消える。
	limitTarget := "stall:filing-check-limit-" + limitDraftID
	response = call("POST", "/api/desk-cleanup/inspect", "screen", `{"target":"`+limitTarget+`"}`)
	var inspected map[string]any
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &inspected) != nil || inspected["fingerprint"] == nil {
		t.Fatal("上限通知を照合できません", response.Code, response.Body.String())
	}
	closeBody, _ := json.Marshal(map[string]any{"target": limitTarget, "fingerprint": inspected["fingerprint"], "reason": "確認済み", "userStatement": "上限通知を非表示にして"})
	if response = call("POST", "/api/desk-cleanup/close", "screen", string(closeBody)); response.Code != 200 {
		t.Fatal("上限通知を非表示にできません", response.Code, response.Body.String())
	}
	if response = call("GET", "/api/stalls/alerts", "screen", ""); response.Code != 200 || strings.Contains(response.Body.String(), "filing-check-limit-"+limitDraftID) {
		t.Fatal("非表示にした上限通知が残っています", response.Code)
	}
	stored, found, e := app.Records.Get(ctx, "todos", todoID)
	if e != nil || !found || stored["text"] != "保存済みのTODO" || stored["extra"].(map[string]any)["unknown"] != "keep" {
		t.Fatalf("saved state changed %v %v", stored, e)
	}
	app.Close()
	app, e = OpenApp(ctx, config, pg, strings.Repeat("e", 40), func(string) {})
	if e != nil {
		t.Fatal(e)
	}
	defer app.Close()
	// コードの版が変わっても（codeRevisionChanged が真になっても）、非表示にした上限通知は出直さない。
	restarted, e := app.Operations()
	if e != nil {
		t.Fatal(e)
	}
	if stalls, e := restarted.Stalls(ctx); e != nil || strings.Contains(fmt.Sprint(stalls), "filing-check-limit-"+limitDraftID) {
		t.Fatal("版の更新で非表示の上限通知が出直しました", e)
	}
	archivedView := *restarted
	archivedView.IncludeArchived = true
	stalls, e := archivedView.Stalls(ctx)
	if e != nil {
		t.Fatal(e)
	}
	// 一覧の行は種類ごとに型が異なるので、HTTPと同じJSONの形で読む。
	var stallJSON []map[string]any
	if raw, e := json.Marshal(stalls); e != nil || json.Unmarshal(raw, &stallJSON) != nil {
		t.Fatal(e)
	}
	changed := false
	for _, row := range stallJSON {
		if row["id"] == "filing-check-limit-"+limitDraftID && row["codeRevisionChanged"] == true {
			changed = true
		}
	}
	if !changed {
		t.Fatal("再起動後の上限通知がコードの版の変化を示しません", stalls)
	}
	stored, found, e = app.Records.Get(ctx, "todos", todoID)
	if e != nil || !found || stored["extra"].(map[string]any)["unknown"] != "keep" {
		t.Fatalf("restart changed saved state %v %v", stored, e)
	}
	if writes.Load() != 0 {
		t.Fatalf("reading APIs unexpectedly wrote to Gitea: %d", writes.Load())
	}
}
