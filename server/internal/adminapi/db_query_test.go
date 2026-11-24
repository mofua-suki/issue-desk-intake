package adminapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/store"
)

const dbQueryPath = "/api/diagnostics/db-query"

func newDBQueryFixture(t *testing.T) *fixture {
	t.Helper()
	pg := newDBQueryDatabase(t)
	// Use both production pools on the same database and exact schema names.
	testURL, err := url.Parse(os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	var database string
	if err := pg.QueryRow("SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	testURL.Path = "/" + database
	db, err := store.OpenApplicationPostgres(context.Background(), testURL.String(), store.ApplicationPostgresSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{t: t, db: db, pg: pg, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), directory: t.TempDir(), token: map[string]string{}}
	for i, name := range []string{"worker", "delegate"} {
		token := "idk_" + strings.Repeat(string(rune('a'+i)), 43)
		f.token[name] = token
		id := []string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}[i]
		if _, err := db.AddPCToken(context.Background(), store.PCTokenRecord{ID: id, Name: name, Hash: auth.Hash(token), IssuedAt: f.now}); err != nil {
			t.Fatal(err)
		}
		if name == "delegate" {
			if _, err := db.SetDelegated(context.Background(), id, true, f.now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.CreateSession(context.Background(), auth.Hash("screen-secret"), auth.Session{CSRF: "screen-csrf", ExpiresAt: f.now.Add(time.Hour)}, f.now); err != nil {
		t.Fatal(err)
	}
	authenticator, err := auth.New(auth.Config{PublicURL: "http://127.0.0.1:8787", Now: func() time.Time { return f.now }}, authStore{db})
	if err != nil {
		t.Fatal(err)
	}
	f.options = Options{AppDB: db.DB(), Postgres: pg, Auth: authenticator, Now: func() time.Time { return f.now }, RuntimeDir: f.directory}
	f.restart()
	return f
}

func dbQueryGrant(t *testing.T, f *fixture, target string) string {
	t.Helper()
	id := strings.ReplaceAll(randomID(), "-", "")
	code, value := f.call("POST", "/api/permission-grants", "delegate", map[string]any{
		"id": id, "kind": "db-read", "target": target, "ttlSeconds": 600,
		"evidence": map[string]any{"quote": "このDBを参照してよい", "statedAt": iso(f.now)},
	})
	status(t, code, 201, value)
	return id
}

func dbQueryInput(target, query string) map[string]any {
	return map[string]any{"target": target, "sql": query}
}

func dbQueryAuditRows(t *testing.T, f *fixture) []map[string]any {
	t.Helper()
	rows, err := queryMaps(context.Background(), f.db.DB(), "SELECT actor, action, target FROM admin_audit WHERE action='db-query' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestDBQueryGrant(t *testing.T) {
	f := newDBQueryFixture(t)
	observed, observation := observeDBQuery(t, f.pg)
	f.pg = observed
	f.restart()
	for _, tc := range []struct{ name, kind, target, state string }{
		{"none", "", "shared", ""},
		{"expired", "db-read", "shared", "expired"},
		{"revoked", "db-read", "shared", "revoked"},
		{"other_target", "db-read", "shared", "active"},
		{"other_kind", "host-ssh", "shared", "active"},
		{"permanent", "db-read", "shared", "permanent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.pg.Exec("DELETE FROM permission_grants"); err != nil {
				t.Fatal(err)
			}
			if tc.kind != "" {
				var expires, revoked any = f.now.Add(time.Hour), nil
				if tc.state == "expired" {
					expires = f.now.Add(-time.Second)
				}
				if tc.state == "revoked" {
					revoked = f.now
				}
				if tc.state == "permanent" {
					expires = nil
				}
				_, err := f.pg.Exec(`INSERT INTO permission_grants(id,kind,target,expires_at,revoked_at,evidence_quote,evidence_stated_at,created_by,created_at) VALUES('unusable',$1,$2,$3,$4,'test',$5,'test',$5)`, tc.kind, tc.target, expires, revoked, f.now)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := len(dbQueryAuditRows(t, f))
			beforeQueries := observation.queries.Load()
			target := "shared"
			if tc.name == "other_target" {
				target = "app"
			}
			// The driver observer proves that rejected requests execute no user SQL.
			code, value := f.call("POST", dbQueryPath, "delegate", dbQueryInput(target, "SELECT 1 / 0"))
			status(t, code, 403, value)
			if observation.queries.Load() != beforeQueries {
				t.Fatal("許可なしでSQLを実行しました")
			}
			rows := dbQueryAuditRows(t, f)
			if len(rows) != before+1 {
				t.Fatalf("拒否の監査が1行ではありません: %d", len(rows)-before)
			}
			var detail map[string]any
			if err := json.Unmarshal([]byte(str(rows[len(rows)-1]["target"])), &detail); err != nil {
				t.Fatal(err)
			}
			if detail["status"] != float64(403) {
				t.Fatalf("拒否の監査: %v", detail)
			}
		})
	}
	if _, err := f.pg.Exec("DELETE FROM permission_grants"); err != nil {
		t.Fatal(err)
	}
	id := dbQueryGrant(t, f, "shared")
	for _, tc := range []struct {
		credential string
		want       int
	}{{"delegate", 200}, {"cli", 200}, {"screen", 403}, {"worker", 403}, {"hostd", 403}, {"", 401}} {
		t.Run(tc.credential, func(t *testing.T) {
			before := len(dbQueryAuditRows(t, f))
			beforeQueries := observation.queries.Load()
			code, value := f.call("POST", dbQueryPath, tc.credential, dbQueryInput("shared", "SELECT count(*) FROM jobs"), map[string]string{"Origin": "http://127.0.0.1:8787"})
			status(t, code, tc.want, value)
			if tc.want == 403 && (observation.queries.Load() != beforeQueries || len(dbQueryAuditRows(t, f)) != before+1) {
				t.Fatal("認可拒否でSQLが実行されたか監査が不足しています")
			}
			if tc.credential == "hostd" && dbQueryAuditRows(t, f)[before]["actor"] != "hostd:test-host" {
				t.Fatal("hostdの拒否の監査に送り手がありません")
			}
		})
	}
	code, value := f.call("POST", "/api/permission-grants/"+id+"/revoke", "delegate", nil)
	status(t, code, 200, value)
	code, value = f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT 1"))
	status(t, code, 403, value)
	dbQueryGrant(t, f, "app")
	code, value = f.call("POST", dbQueryPath, "delegate", dbQueryInput("app", "SELECT count(*) FROM drafts"))
	status(t, code, 200, value)
	code, value = f.call("POST", dbQueryPath, "delegate", dbQueryInput("app", "SELECT 1"))
	status(t, code, 200, value)
}

func TestDBQueryPostgresRejectsSideEffects(t *testing.T) {
	f := newDBQueryFixture(t)
	dbQueryGrant(t, f, "shared")
	if _, err := f.pg.Exec("INSERT INTO jobs(repo,issue,title) VALUES('test/repo',1,'intact')"); err != nil {
		t.Fatal(err)
	}
	other, err := f.pg.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var pid int
	if err := other.QueryRowContext(context.Background(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var beforeSequence int64
	if err := f.pg.QueryRow("SELECT last_value FROM jobs_id_seq").Scan(&beforeSequence); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, query string }{
		{"insert", "INSERT INTO jobs(repo,issue,title) VALUES('test/repo',2,'bad')"},
		{"update", "UPDATE jobs SET title='bad'"}, {"delete", "DELETE FROM jobs"},
		{"merge", "MERGE INTO jobs USING jobs j ON true WHEN MATCHED THEN DELETE"},
		{"create", "CREATE TABLE bad(n int)"}, {"drop", "DROP TABLE jobs"},
		{"copy", "COPY jobs TO STDOUT"}, {"into", "SELECT * INTO bad FROM jobs"},
		{"two", "SELECT 1; SELECT 2"}, {"update_lock", "SELECT * FROM jobs FOR UPDATE"},
		{"share_lock", "SELECT * FROM jobs FOR SHARE"},
		{"cte", "WITH changed AS (DELETE FROM jobs RETURNING *) SELECT * FROM changed"},
		{"sleep", "SELECT pg_sleep(1)"}, {"advisory", "SELECT pg_advisory_lock(1650)"},
		{"terminate", fmt.Sprintf("SELECT pg_terminate_backend(%d)", pid)},
		{"config", "SELECT set_config('application_name','bad',false)"},
		{"sequence", "SELECT nextval('jobs_id_seq')"},
		{"catalog", "SELECT * FROM pg_catalog.pg_class"},
		{"schema", "SELECT * FROM issue_desk_app_migration.drafts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, value := f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", tc.query))
			status(t, code, 400, value)
			var count, locks int
			var title string
			var sequence int64
			if err := f.pg.QueryRow("SELECT count(*), min(title) FROM jobs").Scan(&count, &title); err != nil {
				t.Fatal(err)
			}
			if err := f.pg.QueryRow("SELECT last_value FROM jobs_id_seq").Scan(&sequence); err != nil {
				t.Fatal(err)
			}
			if err := f.pg.QueryRow("SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())").Scan(&locks); err != nil {
				t.Fatal(err)
			}
			var alive int
			if err := other.QueryRowContext(context.Background(), "SELECT 1").Scan(&alive); err != nil {
				t.Fatal(err)
			}
			if count != 1 || title != "intact" || sequence != beforeSequence || locks != 0 || alive != 1 {
				t.Fatalf("副作用: count=%d title=%s sequence=%d locks=%d alive=%d", count, title, sequence, locks, alive)
			}
		})
	}
}

func testDBQuerySecretReferences(t *testing.T) {
	f := newDBQueryFixture(t)
	dbQueryGrant(t, f, "shared")
	dbQueryGrant(t, f, "app")
	secret := "query-test-sensitive-value"
	for _, statement := range []string{
		"INSERT INTO host_image_tokens(host,hash) VALUES('test','" + secret + "')",
		"INSERT INTO settings(key,value) VALUES('test','\"" + secret + "\"')",
		"INSERT INTO ssh_tickets(id,host,operation,reason,evidence,emergency,requester,requested_token,requested_at) VALUES('test','test','status','test','test',false,'test','" + secret + "',now())",
		"INSERT INTO issue_desk_app_migration.settings(id,json) VALUES('test','\"" + secret + "\"')",
		"INSERT INTO issue_desk_app_migration.api_tokens(id,name,hash,issued_at) VALUES('test','test','" + secret + "','test')",
		"INSERT INTO issue_desk_app_migration.cli_keys(hash,expires) VALUES('" + secret + "',1)",
		"INSERT INTO issue_desk_app_migration.temporary_code_records(repo,item,stage,json) VALUES('test','test','test','\"" + secret + "\"')",
	} {
		if _, err := f.pg.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"shared", "app"} {
		tables := []string{"settings", "host_image_tokens", "ssh_tickets"}
		if target == "app" {
			tables = []string{"settings", "api_tokens", "api_token_grants", "cli_keys", "temporary_code_records"}
		}
		for _, table := range tables {
			for _, projection := range []string{"*", "s.*", "s"} {
				query := "SELECT " + projection + " FROM " + table + " s"
				code, value := f.call("POST", dbQueryPath, "delegate", dbQueryInput(target, query))
				status(t, code, 400, value)
				raw, _ := json.Marshal(value)
				if strings.Contains(string(raw), secret) {
					t.Fatal("秘密が応答に含まれます")
				}
			}
		}
	}
	code, value := f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT requested_token AS value FROM ssh_tickets"))
	status(t, code, 400, value)
}

func TestDBQueryAudit(t *testing.T) {
	f := newDBQueryFixture(t)
	id := dbQueryGrant(t, f, "shared")
	if _, err := f.pg.Exec("INSERT INTO jobs(repo,issue,title) VALUES('test/repo',1,'result-only-value')"); err != nil {
		t.Fatal(err)
	}
	queries := []string{"SELECT title FROM jobs; -- original comment", "SELECT pg_sleep(1)"}
	for i, query := range queries {
		code, value := f.call("POST", dbQueryPath, "delegate", dbQueryInput("shared", query))
		status(t, code, []int{200, 400}[i], value)
	}
	rows := dbQueryAuditRows(t, f)
	if len(rows) != 2 {
		t.Fatalf("監査が2行ではありません: %v", rows)
	}
	for i, row := range rows {
		var detail map[string]any
		if err := json.Unmarshal([]byte(str(row["target"])), &detail); err != nil {
			t.Fatal(err)
		}
		if row["actor"] != "token:delegate" || row["action"] != "db-query" || detail["grantId"] != id || detail["target"] != "shared" || detail["sql"] != queries[i] || detail["status"] != float64([]int{200, 400}[i]) {
			t.Fatalf("監査内容: %v %v", row, detail)
		}
		if strings.Contains(str(row["target"]), "result-only-value") {
			t.Fatal("結果の値が監査に含まれます")
		}
	}
	// Read records again after re-registering the service, as a consumer does.
	f.restart()
	if len(dbQueryAuditRows(t, f)) != 2 {
		t.Fatal("監査を再読込できません")
	}
}

// Refused requests may carry up to the 1 MiB body limit; the audit keeps a
// bounded prefix with the original size and digest instead.
func TestDBQueryAuditLongSQL(t *testing.T) {
	f := newDBQueryFixture(t)
	dbQueryGrant(t, f, "shared")
	for i, tc := range []struct {
		credential, query string
		want              int
	}{
		{"delegate", "SELECT '" + strings.Repeat("x", 70000) + "'", 400},
		{"worker", "SELECT '" + strings.Repeat("受付", 30000) + "'", 403},
		{"delegate", "SELECT '" + strings.Repeat("x", 65536-9) + "'", 200},
	} {
		code, value := f.call("POST", dbQueryPath, tc.credential, dbQueryInput("shared", tc.query))
		status(t, code, tc.want, value)
		rows, err := queryMaps(context.Background(), f.db.DB(), "SELECT target FROM admin_audit WHERE action='db-query' ORDER BY id")
		if err != nil || len(rows) != i+1 {
			t.Fatalf("監査: %v %d", err, len(rows))
		}
		var detail map[string]any
		if err := json.Unmarshal([]byte(str(rows[i]["target"])), &detail); err != nil {
			t.Fatal(err)
		}
		saved := str(detail["sql"])
		if len(tc.query) <= 65536 {
			if saved != tc.query || detail["sqlBytes"] != nil || detail["sqlSha256"] != nil {
				t.Fatalf("上限以内のSQLは全文を残します: %d bytes", len(saved))
			}
			continue
		}
		digest := sha256.Sum256([]byte(tc.query))
		if len(saved) > 65536 || len(saved) < 65533 || !strings.HasPrefix(tc.query, saved) || !utf8.ValidString(saved) ||
			detail["sqlBytes"] != float64(len(tc.query)) || detail["sqlSha256"] != hex.EncodeToString(digest[:]) {
			t.Fatalf("長いSQLの監査: saved=%d bytes=%v sha=%v", len(saved), detail["sqlBytes"], detail["sqlSha256"])
		}
	}
}

// A checked-out connection is a real pool wait, not a synthetic handler delay.
func TestDBQueryGrantRecheckedAfterConnectionWait(t *testing.T) {
	f := newDBQueryFixture(t)
	id := dbQueryGrant(t, f, "shared")
	f.pg.SetMaxOpenConns(1)
	held, err := f.pg.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	done := make(chan int, 1)
	go func() {
		code, _, err := f.callResult("POST", dbQueryPath, "delegate", dbQueryInput("shared", "SELECT 1"))
		if err != nil {
			done <- 0
		} else {
			done <- code
		}
	}()
	deadline := time.Now().Add(time.Second * 5)
	for f.pg.Stats().WaitCount == 0 {
		if time.Now().After(deadline) {
			t.Fatal("接続待ちに入りません")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := held.ExecContext(context.Background(), "UPDATE permission_grants SET revoked_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	held.Close()
	if code := <-done; code != 403 {
		t.Fatalf("待機後の取り消し: %d", code)
	}
}
