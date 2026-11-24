package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/pgtest"
)

func TestSQLBoundInputIsolationPostgres(t *testing.T) {
	db := pgtest.New(t)
	ctx := context.Background()
	hostile := `owner/repo' OR true; DELETE FROM decisions; --`
	for _, row := range []struct{ id, project string }{{"ordinary", "owner/repo"}, {"hostile", hostile}} {
		raw, _ := json.Marshal(map[string]any{"id": row.id, "project": row.project, "text": hostile})
		if _, err := db.Exec(`INSERT INTO decisions(id,json) VALUES(?,?)`, row.id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	before, err := jsonList(ctx, tx, "decisions")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ project, id string }{{"owner/repo", "ordinary"}, {hostile, "hostile"}, {`' OR '1'='1`, ""}} {
		result, err := jsonPage(ctx, tx, "decisions", decisionsWhere(), decisionsOrder(), []any{tc.project, tc.project}, contract.Pagination{Page: 1, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if tc.id != "" {
			want = 1
		}
		if len(result.Items) != want || want == 1 && result.Items[0]["id"] != tc.id {
			t.Fatalf("bound project %q read other rows: %#v", tc.project, result)
		}
	}
	after, err := jsonList(ctx, tx, "decisions")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("query changed stored records: %#v %v", after, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestTokenGrantRemovalStaysWithinRequestedToken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	target := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	other := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	if _, err := f.db.SetDelegated(ctx, target, true, f.now); err != nil {
		t.Fatal(err)
	}
	code, value := f.call(http.MethodPost, "/api/tokens/"+target+"/delegate", "screen", map[string]any{"delegate": false})
	status(t, code, 200, value)
	var targetCount, otherCount int
	if err := f.db.DB().QueryRow(`SELECT count(*) FILTER (WHERE token_id=?),count(*) FILTER (WHERE token_id=?) FROM api_token_grants`, target, other).Scan(&targetCount, &otherCount); err != nil || targetCount != 0 || otherCount != 1 {
		t.Fatalf("grant deletion escaped target: %d %d %v", targetCount, otherCount, err)
	}
	code, value = f.call(http.MethodPost, "/api/tokens/not-a-uuid/delegate", "screen", map[string]any{"delegate": false})
	status(t, code, 404, value)
	if err := f.db.DB().QueryRow(`SELECT count(*) FROM api_token_grants WHERE token_id=?`, other).Scan(&otherCount); err != nil || otherCount != 1 {
		t.Fatalf("invalid request removed another grant: %d %v", otherCount, err)
	}
}
