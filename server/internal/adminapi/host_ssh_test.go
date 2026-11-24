package adminapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/store"
)

func repairFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("専用PostgreSQL DSNを指定して実DBの修理API試験を行う")
	}
	ctx := context.Background()
	admin, e := sql.Open("pgx", dsn)
	if e != nil {
		t.Fatal(e)
	}
	schema := "admin_" + strings.ReplaceAll(randomID(), "-", "")
	if _, e = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	parsed, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, e := sql.Open("pgx", parsed.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		db.Close()
		_, e := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		if e != nil {
			t.Error(e)
		}
	})
	if _, e = store.Migrate(ctx, db); e != nil {
		t.Fatal(e)
	}
	f := newFixture(t)
	f.pg = db
	f.restart()
	return f
}

func TestSSHDistributedPolicyIssuedCommandFiltering(t *testing.T) {
	f := repairFixture(t)
	_, policy := distributedReadOnlyPolicy(t)
	policy["signer"] = map[string]any{"token": "signer"}
	policy["hosts"] = map[string]any{"test-host": map[string]any{"principal": "issue-desk-repair-test-host", "user": "test", "machine": "test-machine.example.ts.net"}}
	delete(policy, "operationHosts")
	delete(policy, "directPRVerification")
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.policy, raw, 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'ordinary output\\nidk_secret\\nPASSWORD value\\n-----BEGIN PRIVATE KEY-----\\nkey body\\n-----END PRIVATE KEY-----\\nafter key\\n'\nprintf 'ordinary stderr\\ntoken value\\n' >&2\n"
	for _, name := range []string{"go", "node", "docker"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range []string{"tools", "worker-logs"} {
		t.Run(operation, func(t *testing.T) {
			code, value := f.call("POST", "/api/ssh-tickets", "delegate", ticketInput(operation))
			status(t, code, 201, value)
			if obj(value)["approver"] != "policy:status" {
				t.Fatal(value)
			}
			id := str(obj(value)["id"])
			code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
			status(t, code, 200, value)
			cmd := exec.Command("/bin/sh", "-c", str(obj(value)["forceCommand"]))
			cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "BOOTSTRAP_DIR="+dir)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("issued command: %v: %s", err, out)
			}
			for _, safe := range []string{"ordinary output", "ordinary stderr", "after key"} {
				if !strings.Contains(string(out), safe) {
					t.Fatalf("missing %q: %s", safe, out)
				}
			}
			for _, secret := range []string{"idk_secret", "PASSWORD", "PRIVATE KEY", "key body", "token value"} {
				if strings.Contains(string(out), secret) {
					t.Fatalf("leaked %q: %s", secret, out)
				}
			}
		})
	}
}
func grantInput(f *fixture, id, kind, target string) map[string]any {
	return map[string]any{"id": id, "kind": kind, "target": target, "ttlSeconds": 60, "evidence": map[string]any{"quote": "テストの許可", "statedAt": iso(f.now)}}
}
func TestPermissionGrantAtomicityExpiryAndRevoke(t *testing.T) {
	f := repairFixture(t)
	input := grantInput(f, "11111111111111111111111111111111", "host-ssh", "test-host:repair")
	code, value := f.call("POST", "/api/permission-grants", "worker", input)
	status(t, code, 403, value)
	code, value = f.call("POST", "/api/permission-grants", "delegate", input)
	status(t, code, 201, value)
	expires := obj(value)["expiresAt"]
	code, value = f.call("POST", "/api/permission-grants", "delegate", input)
	status(t, code, 200, value)
	if obj(value)["expiresAt"] != expires {
		t.Fatal("再送で期限が延長しました")
	}
	code, value = f.call("POST", "/api/permission-grants/11111111111111111111111111111111/applied", "screen", map[string]any{})
	status(t, code, 403, value)
	code, value = f.call("POST", "/api/permission-grants/11111111111111111111111111111111/applied", "delegate", map[string]any{})
	status(t, code, 200, value)
	input["target"] = "test-host:status"
	code, value = f.call("POST", "/api/permission-grants", "delegate", input)
	status(t, code, 409, value)
	f.now = f.now.Add(time.Minute)
	code, value = f.call("GET", "/api/permission-grants", "screen", nil)
	status(t, code, 200, value)
	if len(obj(value)["grants"].([]any)) != 0 {
		t.Fatal("期限同値で有効扱いしました")
	}
	code, value = f.call("POST", "/api/permission-grants/11111111111111111111111111111111/applied", "delegate", map[string]any{})
	status(t, code, 409, value)
	f.restart()
	code, value = f.call("GET", "/api/permission-grants/11111111111111111111111111111111", "screen", nil)
	status(t, code, 200, value)
	if obj(value)["status"] != "expired" {
		t.Fatal(value)
	}
	var group sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			id := []string{"22222222222222222222222222222222", "33333333333333333333333333333333"}[index]
			code, _ := f.call("POST", "/api/permission-grants", "delegate", grantInput(f, id, "hostd-repair", "test-host:set-node-path"))
			codes <- code
		}(i)
	}
	group.Wait()
	close(codes)
	created, conflicts := 0, 0
	for code := range codes {
		if code == 201 {
			created++
		}
		if code == 409 {
			conflicts++
		}
	}
	if created != 1 || conflicts != 1 {
		t.Fatalf("created=%d conflicts=%d", created, conflicts)
	}
	code, value = f.call("GET", "/api/permission-grants", "screen", nil)
	status(t, code, 200, value)
	grant := obj(obj(value)["grants"].([]any)[0])
	id := str(grant["id"])
	code, value = f.call("POST", "/api/permission-grants/"+id+"/revoke", "delegate", map[string]any{})
	status(t, code, 200, value)
	revoked := obj(value)["revokedAt"]
	code, value = f.call("POST", "/api/permission-grants/"+id+"/revoke", "delegate", map[string]any{})
	status(t, code, 200, value)
	if obj(value)["revokedAt"] != revoked {
		t.Fatal("再送で取り消し時刻が変わりました")
	}
}
func ticketInput(operation string) map[string]any {
	return map[string]any{"host": "test-host", "operation": operation, "reason": "SSH接続を確認する", "evidence": "テスト用の根拠", "emergency": false}
}
func TestSSHTicketScreenApprovalSignerAndResultOwner(t *testing.T) {
	f := repairFixture(t)
	code, value := f.call("POST", "/api/ssh-tickets", "worker", ticketInput("repair"))
	status(t, code, 201, value)
	ticket := obj(value)
	id := str(ticket["id"])
	if ticket["status"] != "requested" {
		t.Fatal(ticket)
	}
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/approve", "delegate", map[string]any{})
	status(t, code, 403, value)
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
	status(t, code, 409, value)
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/approve", "screen", map[string]any{})
	status(t, code, 200, value)
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "worker", map[string]any{})
	status(t, code, 403, value)
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
	status(t, code, 200, value)
	if obj(value)["principal"] != "issue-desk-repair-test-host" || !strings.Contains(str(obj(value)["forceCommand"]), "echo repair") {
		t.Fatal(value)
	}
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
	status(t, code, 409, value)
	result := map[string]any{"connectionResult": "connected", "exitCode": 0, "summary": "Authorization: Bearer secret-value token=another-secret 完了"}
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/result", "delegate", result)
	status(t, code, 403, value)
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/result", "worker", result)
	status(t, code, 200, value)
	if strings.Contains(str(obj(value)["summary"]), "secret-value") || strings.Contains(str(obj(value)["summary"]), "another-secret") {
		t.Fatal("秘密が結果に残りました")
	}
	f.restart()
	code, value = f.call("GET", "/api/ssh-tickets/"+id, "screen", nil)
	status(t, code, 200, value)
	if obj(value)["status"] != "reported" {
		t.Fatal(value)
	}
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/result", "worker", result)
	status(t, code, 409, value)
}
func TestSSHExpiryGrantApprovalAndCurrentPolicy(t *testing.T) {
	f := repairFixture(t)
	code, value := f.call("POST", "/api/permission-grants", "delegate", grantInput(f, "44444444444444444444444444444444", "host-ssh", "test-host:repair"))
	status(t, code, 201, value)
	code, value = f.call("POST", "/api/ssh-tickets", "worker", ticketInput("repair"))
	status(t, code, 201, value)
	if obj(value)["status"] != "requested" {
		t.Fatal("代行なしのトークンで付与を使いました")
	}
	// 読むだけの操作（status）でも自動承認しない。付与が無ければ承認待ち。
	code, value = f.call("POST", "/api/ssh-tickets", "delegate", ticketInput("status"))
	status(t, code, 201, value)
	if obj(value)["status"] != "requested" || obj(value)["approver"] != nil {
		t.Fatal("付与の無い申請を承認しました", value)
	}
	code, value = f.call("POST", "/api/ssh-tickets", "delegate", ticketInput("repair"))
	status(t, code, 201, value)
	id := str(obj(value)["id"])
	if obj(value)["approver"] != "grant:44444444444444444444444444444444" {
		t.Fatal(value)
	}
	f.now = f.now.Add(5 * time.Minute)
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
	status(t, code, 409, value)
	code, value = f.call("GET", "/api/ssh-tickets/"+id, "screen", nil)
	status(t, code, 200, value)
	if obj(value)["status"] != "expired" {
		t.Fatal(value)
	}
	// 有事でも時間が経っても、承認の無い申請は発券しない（自動承認は無い）。
	emergency := ticketInput("repair")
	emergency["emergency"] = true
	code, value = f.call("POST", "/api/ssh-tickets", "worker", emergency)
	status(t, code, 201, value)
	id = str(obj(value)["id"])
	f.now = f.now.Add(15 * time.Minute)
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
	status(t, code, 409, value)
	if e := os.Remove(f.policy); e != nil {
		t.Fatal(e)
	}
	code, value = f.call("GET", "/api/ssh-tickets/"+id, "screen", nil)
	status(t, code, 503, value)
}

// TestPermissionGrantListFiltering compares the shipped list route with the former
// full-read implementation, including the timestamp conversion in queryMaps.
func TestPermissionGrantListFiltering(t *testing.T) {
	f := repairFixture(t)
	ctx := context.Background()
	_, err := f.pg.ExecContext(ctx, `INSERT INTO permission_grants
 (id,kind,target,expires_at,evidence_quote,evidence_stated_at,created_by,created_at,revoked_at)
 SELECT lpad(n::text,32,'0'), CASE WHEN n%2=0 THEN 'host-ssh' ELSE 'hostd-repair' END,
 'fixture', $1::timestamptz - interval '1 day', 'fixture', $1, 'fixture', $1,
 CASE WHEN n%2=0 THEN $1::timestamptz ELSE NULL END FROM generate_series(1,10000) n`, f.now)
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range []struct {
		kind             string
		expires, revoked any
	}{
		{"nas-claude-rule", nil, nil}, {"host-ssh", f.now.Add(time.Minute), nil},
		{"host-ssh", f.now, nil}, {"hostd-repair", f.now, f.now},
		{"host-ssh", f.now.Add(500 * time.Microsecond), nil},
		{"host-ssh", f.now.Add(time.Minute), f.now},
		{"nas-claude-rule", nil, f.now},
	} {
		_, err = f.pg.ExecContext(ctx, `INSERT INTO permission_grants
  (id,kind,target,expires_at,evidence_quote,evidence_stated_at,created_by,created_at,revoked_at)
  VALUES ($1,$2,'fixture',$3,'fixture',$4,'fixture',$4,$5)`, fmt.Sprintf("special-%02d", i), item.kind, item.expires, f.now, item.revoked)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pg.ExecContext(ctx, "UPDATE permission_grants SET created_at = created_at - interval '1 day' WHERE id = 'special-00'"); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pg.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	runner := &repairTx{tx}
	measure := func(fn func() ([]map[string]any, error)) ([]map[string]any, time.Duration, uint64) {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		rows, err := fn()
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		return rows, elapsed, after.TotalAlloc - before.TotalAlloc
	}
	old, oldTime, oldAlloc := measure(func() ([]map[string]any, error) {
		return queryMaps(ctx, runner, "SELECT * FROM permission_grants ORDER BY created_at DESC,id")
	})
	filtered, newTime, newAlloc := measure(func() ([]map[string]any, error) { return listGrantRows(ctx, runner, "active", "", f.now) })
	if len(old) != 10007 || len(filtered) != 2 {
		t.Fatalf("read counts old=%d new=%d", len(old), len(filtered))
	}
	t.Logf("representative full-read: rows=%d duration=%s allocated=%d bytes; filtered: rows=%d duration=%s allocated=%d bytes", len(old), oldTime, oldAlloc, len(filtered), newTime, newAlloc)
	for _, tc := range []struct{ query, status, kind string }{
		{"", "active", ""}, {"?status=active&kind=host-ssh", "active", "host-ssh"},
		{"?status=all", "all", ""}, {"?status=all&kind=hostd-repair", "all", "hostd-repair"},
	} {
		expected := []map[string]any{}
		for _, row := range old {
			if (tc.kind == "" || row["kind"] == tc.kind) && (tc.status == "all" || grantStatus(row, f.now) == "active") {
				expected = append(expected, grantView(row, f.now))
			}
		}
		code, got := f.call("GET", "/api/permission-grants"+tc.query, "screen", nil)
		status(t, code, 200, got)
		if tc.kind == "" {
			seen := map[string]string{}
			for _, value := range obj(got)["grants"].([]any) {
				grant := obj(value)
				seen[str(grant["id"])] = str(grant["status"])
			}
			for _, id := range []string{"special-05", "special-06"} {
				state, present := seen[id]
				if tc.status == "active" && present || tc.status == "all" && (!present || state != "revoked") {
					t.Errorf("%s: grant %s present=%t status=%q", tc.query, id, present, state)
				}
			}
		}
		// The HTTP boundary converts Go values to JSON values.
		raw, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		var want any
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(obj(got)["grants"], want) {
			t.Fatalf("%s: list differs from legacy", tc.query)
		}
	}
	for _, query := range []string{"?status=expired", "?status=ACTIVE", "?kind=unknown", "?status=all&kind=unknown"} {
		code, got := f.call("GET", "/api/permission-grants"+query, "screen", nil)
		status(t, code, 400, got)
	}
}

func TestSSHReadOnlyApprovalAndCurrentPolicy(t *testing.T) {
	f := repairFixture(t)
	_, policy := distributedReadOnlyPolicy(t)
	policy["ttlMinutes"] = 5
	policy["signer"] = map[string]any{"token": "signer"}
	policy["hosts"] = map[string]any{"test-host": map[string]any{"principal": "issue-desk-repair-test-host", "user": "test", "machine": "test-machine.example.ts.net"}}
	delete(policy, "operationHosts")
	delete(policy, "directPRVerification")
	save := func() {
		raw, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(f.policy, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	save()
	code, value := f.call("POST", "/api/permission-grants", "delegate", grantInput(f, "66666666666666666666666666666666", "host-ssh", "test-host:tools"))
	status(t, code, 201, value)
	for i, credential := range []string{"delegate", "delegate", "cli"} {
		code, value := f.call("POST", "/api/ssh-tickets", credential, ticketInput("tools"))
		status(t, code, 201, value)
		ticket := obj(value)
		id := str(ticket["id"])
		if ticket["status"] != "approved" || ticket["approver"] != "policy:status" || ticket["decided_at"] != iso(f.now) {
			t.Fatal(ticket)
		}
		code, value = f.call("GET", "/api/ssh-tickets/"+id, "screen", nil)
		status(t, code, 200, value)
		if obj(value)["approver"] != "policy:status" {
			t.Fatal(value)
		}
		code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "worker", map[string]any{})
		status(t, code, 403, value)
		code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
		status(t, code, 200, value)
		if obj(value)["forceCommand"] != obj(policy["operations"])["tools"] {
			t.Fatal(value)
		}
		code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
		status(t, code, 409, value)
		if i == 0 {
			code, value = f.call("POST", "/api/permission-grants/66666666666666666666666666666666/revoke", "delegate", map[string]any{})
			status(t, code, 200, value)
		}
	}
	for _, tc := range []struct{ credential, operation string }{{"screen", "tools"}, {"worker", "tools"}, {"delegate", "verify-intake"}, {"cli", "verify-intake"}} {
		code, value := f.call("POST", "/api/ssh-tickets", tc.credential, ticketInput(tc.operation))
		status(t, code, 201, value)
		if obj(value)["status"] != "requested" || obj(value)["approver"] != nil {
			t.Fatal(value)
		}
	}
	for _, input := range []map[string]any{ticketInput("unknown"), {"host": "unknown", "operation": "tools", "reason": "x", "evidence": "x", "emergency": false}, {"host": "test-host", "operation": "tools"}} {
		code, value := f.call("POST", "/api/ssh-tickets", "delegate", input)
		status(t, code, 400, value)
	}
	code, value = f.call("POST", "/api/ssh-tickets", "delegate", ticketInput("tools"))
	status(t, code, 201, value)
	id := str(obj(value)["id"])
	policy["readOnlyOperations"] = []string{"status", "worker-logs"}
	save()
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
	status(t, code, 409, value)
	code, value = f.call("POST", "/api/ssh-tickets", "delegate", ticketInput("tools"))
	status(t, code, 201, value)
	screenID := str(obj(value)["id"])
	code, value = f.call("POST", "/api/ssh-tickets/"+screenID+"/approve", "screen", map[string]any{})
	status(t, code, 200, value)
	code, value = f.call("POST", "/api/ssh-tickets/"+screenID+"/issue", "signer", map[string]any{})
	status(t, code, 200, value)
	code, value = f.call("POST", "/api/permission-grants", "delegate", grantInput(f, "55555555555555555555555555555555", "host-ssh", "test-host:tools"))
	status(t, code, 201, value)
	code, value = f.call("POST", "/api/ssh-tickets", "delegate", ticketInput("tools"))
	status(t, code, 201, value)
	grantID := str(obj(value)["id"])
	if obj(value)["approver"] != "grant:55555555555555555555555555555555" {
		t.Fatal(value)
	}
	code, value = f.call("POST", "/api/ssh-tickets/"+grantID+"/issue", "signer", map[string]any{})
	status(t, code, 200, value)
}

func TestSSHOperationHostRestrictions(t *testing.T) {
	f := repairFixture(t)
	raw, err := os.ReadFile(f.policy)
	if err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if err = json.Unmarshal(raw, &policy); err != nil {
		t.Fatal(err)
	}
	hosts := policy["hosts"].(map[string]any)
	hosts["other-host"] = map[string]any{"principal": "issue-desk-repair-other-host", "user": "test", "machine": "other.example.ts.net"}
	save := func(restrictions any) {
		t.Helper()
		if restrictions == nil {
			delete(policy, "operationHosts")
		} else {
			policy["operationHosts"] = restrictions
		}
		data, e := json.Marshal(policy)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(f.policy, data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	policy["readOnlyOperations"] = []string{"status"}
	allowed := map[string]any{"repair": []string{"test-host"}, "status": []string{"test-host"}}
	save(allowed)
	readOnlyOther := ticketInput("status")
	readOnlyOther["host"] = "other-host"
	code, value := f.call("POST", "/api/ssh-tickets", "delegate", readOnlyOther)
	status(t, code, 400, value)
	code, value = f.call("POST", "/api/ssh-tickets", "delegate", ticketInput("status"))
	status(t, code, 201, value)
	if obj(value)["approver"] != "policy:status" {
		t.Fatal(value)
	}
	readOnlyID := str(obj(value)["id"])
	grantID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, target := range []string{"other-host:repair", "unknown:repair", "test-host:unknown"} {
		code, value := f.call("POST", "/api/permission-grants", "delegate", grantInput(f, grantID, "host-ssh", target))
		status(t, code, 400, value)
	}
	code, value = f.call("POST", "/api/permission-grants", "delegate", grantInput(f, grantID, "host-ssh", "test-host:repair"))
	status(t, code, 201, value)
	code, value = f.call("POST", "/api/permission-grants/"+grantID+"/applied", "delegate", map[string]any{})
	status(t, code, 200, value)
	for _, pair := range [][2]string{{"other-host", "repair"}, {"unknown", "repair"}, {"test-host", "unknown"}} {
		input := ticketInput(pair[1])
		input["host"] = pair[0]
		code, value = f.call("POST", "/api/ssh-tickets", "delegate", input)
		status(t, code, 400, value)
	}
	code, value = f.call("POST", "/api/ssh-tickets", "delegate", ticketInput("repair"))
	status(t, code, 201, value)
	if obj(value)["approver"] != "grant:"+grantID {
		t.Fatal(value)
	}
	id := str(obj(value)["id"])
	code, value = f.call("POST", "/api/ssh-tickets/"+id+"/issue", "signer", map[string]any{})
	status(t, code, 200, value)
	code, value = f.call("POST", "/api/ssh-tickets", "worker", ticketInput("repair"))
	status(t, code, 201, value)
	pending := str(obj(value)["id"])
	code, value = f.call("POST", "/api/ssh-tickets", "worker", ticketInput("repair"))
	status(t, code, 201, value)
	approved := str(obj(value)["id"])
	code, value = f.call("POST", "/api/ssh-tickets/"+approved+"/approve", "screen", map[string]any{})
	status(t, code, 200, value)
	save(map[string]any{"repair": []string{"other-host"}, "status": []string{"other-host"}})
	code, value = f.call("POST", "/api/ssh-tickets/"+readOnlyID+"/issue", "signer", map[string]any{})
	status(t, code, 409, value)
	code, value = f.call("POST", "/api/permission-grants/"+grantID+"/applied", "delegate", map[string]any{})
	status(t, code, 409, value)
	code, value = f.call("POST", "/api/ssh-tickets/"+pending+"/approve", "screen", map[string]any{})
	status(t, code, 409, value)
	code, value = f.call("POST", "/api/ssh-tickets/"+approved+"/issue", "signer", map[string]any{})
	status(t, code, 409, value)
	code, value = f.call("POST", "/api/ssh-tickets/"+pending+"/issue", "signer", map[string]any{})
	status(t, code, 409, value)
	save(map[string]any{"repair": []string{"other-host"}})
	input := ticketInput("status")
	input["host"] = "other-host"
	code, value = f.call("POST", "/api/ssh-tickets", "worker", input)
	status(t, code, 201, value)
	for _, invalid := range []any{map[string]any{}, map[string]any{"repair": []string{}}, map[string]any{"repair": nil}, map[string]any{"repair": []string{"test-host", "test-host"}}, map[string]any{"repair": []string{"unknown"}}, map[string]any{"unknown": []string{"test-host"}}, "bad"} {
		save(invalid)
		code, value = f.call("POST", "/api/ssh-tickets", "worker", ticketInput("repair"))
		status(t, code, 503, value)
	}
	policy["operationHosts"] = nil
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.policy, data, 0600); err != nil {
		t.Fatal(err)
	}
	code, value = f.call("POST", "/api/ssh-tickets", "worker", ticketInput("repair"))
	status(t, code, 503, value)
	save(nil)
	code, value = f.call("POST", "/api/ssh-tickets", "worker", ticketInput("repair"))
	status(t, code, 201, value)
}
