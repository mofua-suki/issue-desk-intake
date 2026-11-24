package hosts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/store"
)

func TestImageFailureHistoryPostgres(t *testing.T) {
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN is not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "issue_desk_test") {
		t.Fatal("isolated test database required")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	schema := "images_test_" + hex.EncodeToString(id[:])
	if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	}()
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO host_image_results(host,tag,flavor,digest,status,at) VALUES ('a','tag','','d','failed',now()),('b','tag','','d','failed',now()),('c','tag','','d','success',now()),('d','other','','d','failed',now())`)
	exec(`INSERT INTO host_image_history(host,tag,flavor,digest,status,reason,detail,self_check_failed,received_at) VALUES ('a','tag','','d','failed','old','old','[]',now()-interval '1 hour'),('a','tag','-python','d','failed','wrong flavor','x','[]',now()),('a','other','','d','failed','wrong tag','x','[]',now()),('b','tag','','d','failed','host b','x','[]',now()),('d','other','','d','failed',NULL,NULL,NULL,now())`)
	secret := "token=supersecret " + strings.Repeat("x", 400)
	exec(`INSERT INTO host_image_history(host,tag,flavor,digest,status,stage,reason,detail,self_check_failed) VALUES ('a','tag','','d','failed','swap-start',$1,$1,$2::jsonb)`, secret, `["token=supersecret"]`)
	live := map[string]map[string]any{}
	for _, h := range []string{"a", "b", "c", "d", "empty"} {
		live[h] = map[string]any{"hostd_health": map[string]any{"cycle": map[string]any{"result": "success"}}, "checks": map[string]any{"items": []any{map[string]any{"ok": false, "label": "fallback"}}}}
	}
	snapshots, _, err := (&Service{DB: db}).imageSnapshots(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	failure := func(h string) map[string]any {
		return fleet.Object(fleet.Object(fleet.Object(snapshots[h])["image"])["lastFailure"])
	}
	a := failure("a")
	if a == nil || a["stage"] != "swap-start" || strings.Contains(fleet.Text(a["reason"]), "supersecret") || len([]rune(fleet.Text(a["reason"]))) > 300 || strings.Contains(fleet.Text(fleet.List(a["selfCheckFailed"])[0]), "supersecret") {
		t.Fatalf("unsafe or incorrect history: %#v", a)
	}
	if failure("b")["reason"] != "host b" {
		t.Fatalf("wrong host: %#v", failure("b"))
	}
	if failure("d")["reason"] != nil || fleet.List(failure("d")["selfCheckFailed"])[0] != "fallback" {
		t.Fatalf("fallback: %#v", failure("d"))
	}
	if failure("c") != nil || failure("empty") != nil {
		t.Fatal("success or missing result has failure")
	}
	image := fleet.Object(fleet.Object(snapshots["a"])["image"])
	if len(fleet.List(image["results"])) != 1 || len(fleet.List(image["failedTags"])) != 1 {
		t.Fatalf("result compatibility: %#v", image)
	}
}
