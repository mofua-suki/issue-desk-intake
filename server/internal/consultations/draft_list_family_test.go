package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/pgtest"
)

func familyListFixture(t *testing.T) (pgtest.Env, *Service, *routeReadControl, *http.ServeMux, string, auth.Principal) {
	t.Helper()
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	control := &routeReadControl{}
	db := sql.OpenDB(routeReadConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
	t.Cleanup(func() { db.Close() })
	service, authDB, _ := fixture(t)
	service.options.Records = &Records{DB: db}
	token := stopToken(t, authDB, true)
	req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	principal, authErr := service.options.Auth.Authenticate(req)
	if authErr != nil {
		t.Fatal(authErr)
	}
	mux := http.NewServeMux()
	service.Register(mux)
	return env, service, control, mux, token, principal
}

func familyListRequest(t *testing.T, mux *http.ServeMux, token, query string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts"+query, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 {
		t.Fatalf("status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
	return response
}

func TestDraftListFamilyFullReadCount(t *testing.T) {
	env, _, control, mux, token, _ := familyListFixture(t)
	ctx := context.Background()
	insert := func(id, raw string) {
		t.Helper()
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, id, raw); err != nil {
			t.Fatal(err)
		}
	}
	insert("parent", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["c1","c2","c3","c4"],"splitIssues":[{},{},{},{}]}`)
	for index := 1; index <= 4; index++ {
		id := fmt.Sprintf("c%d", index)
		insert(id, fmt.Sprintf(`{"id":%q,"project":"owner/app","status":"intake_waiting","parentSplit":"parent","body":%q}`, id, strings.Repeat("x", 4096)))
	}
	check := func(query string, expectedRows int) {
		t.Helper()
		*control = routeReadControl{}
		out := familyListRequest(t, mux, token, query)
		expectedCalls := 1
		if expectedRows == 0 {
			expectedCalls = 0
		}
		if len(out["items"].([]any)) != expectedRows || control.familyCalls != expectedCalls || control.familyBytes >= 4096 || control.fullCalls != expectedCalls || control.fullRows != expectedRows || control.singleCalls != 0 || control.fallbackCalls != 0 {
			t.Fatalf("query=%s response=%v counts=%+v", query, out, control)
		}
		t.Logf("%s full: queries=%d rows=%d bytes=%d; single-full: queries=%d rows=%d bytes=%d; fallback-full: queries=%d rows=%d bytes=%d; page-projection: queries=%d rows=%d bytes=%d; family-projection: queries=%d rows=%d bytes=%d", query, control.fullCalls, control.fullRows, control.fullBytes, control.singleCalls, control.singleRows, control.singleBytes, control.fallbackCalls, control.fallbackRows, control.fallbackBytes, control.projectionCalls, control.pageProjectionRows, control.pageProjectionBytes, control.familyCalls, control.familyRows, control.familyBytes)
	}
	check("?status=draft&limit=1", 1)
	check("?status=draft&page=99&limit=1", 0)
	insert("parent2", `{"id":"parent2","project":"owner/app","status":"draft","splitChildren":["c5"],"splitIssues":[{}]}`)
	insert("c5", `{"id":"c5","project":"owner/app","status":"intake_waiting","parentSplit":"parent2"}`)
	check("?status=draft&limit=10", 2)
	check("?limit=3", 3)
}

func TestDraftListFamilyActionsCompatibility(t *testing.T) {
	env, service, control, mux, token, principal := familyListFixture(t)
	ctx := context.Background()
	cases := []struct {
		name, parent, child string
		fail, update        bool
	}{
		{"unfiled", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"],"splitIssues":[{}]}`, `{"id":"child","parentSplit":"parent","status":"draft"}`, false, false},
		{"partial", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"],"splitIssues":[{}]}`, `{"id":"child","parentSplit":"parent","status":"submitted","issue":{"number":7}}`, false, false},
		{"finished child", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"],"splitIssues":[{}]}`, `{"id":"child","parentSplit":"parent","status":"submitted","finishedWithoutIssue":true}`, false, false},
		{"filed arrays", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"],"splitIssues":[{}]}`, `{"id":"child","parentSplit":"parent","status":"draft","investigations":[{}]}`, false, false},
		{"closed", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"]}`, `{"id":"child","parentSplit":"parent","status":"closed"}`, false, false},
		{"processing", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"]}`, `{"id":"child","parentSplit":"parent","status":"thinking"}`, false, false},
		{"missing", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["missing"]}`, `{"id":"child","parentSplit":"parent","status":"draft"}`, false, false},
		{"duplicate", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child","child"]}`, `{"id":"child","parentSplit":"parent","status":"draft"}`, false, false},
		{"invalid", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":[42,""]}`, `{"id":"child","parentSplit":"parent","status":"draft"}`, false, false},
		{"key mismatch", `{"id":"different","project":"owner/app","status":"draft","splitChildren":["child"]}`, `{"id":"other","parentSplit":"different","status":"draft"}`, false, false},
		{"read failure", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"]}`, `{"id":"child","parentSplit":"parent","status":"draft"}`, true, false},
		{"update before family", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"]}`, `{"id":"child","parentSplit":"parent","status":"draft"}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := env.DB.ExecContext(ctx, `DELETE FROM drafts`); err != nil {
				t.Fatal(err)
			}
			for _, row := range []struct{ key, raw string }{{"parent", tc.parent}, {"child", tc.child}} {
				if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, row.key, row.raw); err != nil {
					t.Fatal(err)
				}
			}
			*control = routeReadControl{failFamily: tc.fail}
			if tc.update {
				control.beforeFamily = func() {
					if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json=jsonb_set(json::jsonb,'{status}','"closed"') WHERE id='child'`); err != nil {
						t.Fatal(err)
					}
				}
			}
			out := familyListRequest(t, mux, token, "?project=owner%2Fapp&limit=1")
			items := out["items"].([]any)
			if len(items) != 1 {
				t.Fatal(out)
			}
			selected, err := decode(tc.parent)
			if err != nil {
				t.Fatal(err)
			}
			*control = routeReadControl{failSingle: tc.fail}
			want := service.DraftView(selected, principal, "http://localhost:8787")["actions"]
			encoded, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var expected any
			if err := json.Unmarshal(encoded, &expected); err != nil {
				t.Fatal(err)
			}
			got := items[0].(map[string]any)["actions"]
			if !reflect.DeepEqual(got, expected) {
				t.Fatalf("got=%v want=%v", got, expected)
			}
			if tc.fail {
				for _, action := range got.([]any) {
					if action == "finish" || action == "discard" || action == "resubmit" || action == "todo" {
						t.Fatalf("failure allowed %s", action)
					}
				}
			}
		})
	}
	t.Run("selected child and updated parent", func(t *testing.T) {
		if _, err := env.DB.ExecContext(ctx, `DELETE FROM drafts`); err != nil {
			t.Fatal(err)
		}
		parent := `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child","sibling"],"splitIssues":[{},{}]}`
		child := `{"id":"child","project":"owner/app","parentSplit":"parent","status":"intake_waiting"}`
		sibling := `{"id":"sibling","project":"owner/app","parentSplit":"parent","status":"submitted","issue":{"number":1}}`
		for _, row := range []struct{ key, raw string }{{"parent", parent}, {"child", child}, {"sibling", sibling}} {
			if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, row.key, row.raw); err != nil {
				t.Fatal(err)
			}
		}
		*control = routeReadControl{beforeFull: func() {
			if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json=jsonb_set(json::jsonb,'{status}','"closed"') WHERE id='parent'`); err != nil {
				t.Fatal(err)
			}
		}}
		out := familyListRequest(t, mux, token, "?status=intake_waiting&limit=1")
		items := out["items"].([]any)
		if len(items) != 1 || control.fullCalls != 1 || control.singleCalls != 0 {
			t.Fatalf("response=%v counts=%+v", out, control)
		}
		selected, err := decode(child)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(service.DraftView(selected, principal, "http://localhost:8787")["actions"])
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(items[0].(map[string]any)["actions"])
		if err != nil || string(got) != string(want) {
			t.Fatalf("got=%s want=%s err=%v", got, want, err)
		}
	})
	t.Run("projection fallback after selection", func(t *testing.T) {
		if _, err := env.DB.ExecContext(ctx, `DELETE FROM drafts`); err != nil {
			t.Fatal(err)
		}
		parent := `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"]}`
		child := `{"id":"child","parentSplit":"parent","status":"draft"}`
		for _, row := range []struct{ key, raw string }{{"parent", parent}, {"child", child}} {
			if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, row.key, row.raw); err != nil {
				t.Fatal(err)
			}
		}
		*control = routeReadControl{beforeFamily: func() {
			if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json=$1 WHERE id='child'`, child+" trailing"); err != nil {
				t.Fatal(err)
			}
		}}
		out := familyListRequest(t, mux, token, "?project=owner%2Fapp&limit=1")
		items := out["items"].([]any)
		if len(items) != 1 || control.singleCalls == 0 {
			t.Fatalf("fallback not used: response=%v counts=%+v", out, control)
		}
		t.Logf("exceptional family fallback full: queries=%d rows=%d bytes=%d", control.singleCalls, control.singleRows, control.singleBytes)
		selected, err := decode(parent)
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(service.DraftView(selected, principal, "http://localhost:8787")["actions"])
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(items[0].(map[string]any)["actions"])
		if err != nil || string(got) != string(want) {
			t.Fatalf("got=%s want=%s err=%v", got, want, err)
		}
	})
}

func TestDraftListFamilyTimingHTTP(t *testing.T) {
	env, service, control, mux, token, _ := familyListFixture(t)
	for _, row := range []struct{ id, raw string }{
		{"parent", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"],"splitIssues":[{}]}`},
		{"child", `{"id":"child","parentSplit":"parent","status":"closed"}`},
	} {
		if _, err := env.DB.Exec(`INSERT INTO drafts(id,json) VALUES($1,$2)`, row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	clock := time.Unix(0, 0)
	service.timingNow = func() time.Time { return clock }
	control.beforeFamily = func() { clock = clock.Add(40 * time.Millisecond) }
	req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts?project=owner%2Fapp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Items []struct {
			Actions []string `json:"actions"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Items) != 1 {
		t.Fatalf("response %s: %v", w.Body.String(), err)
	}
	for _, action := range result.Items[0].Actions {
		if action == "finish" || action == "discard" {
			t.Fatalf("closed child allowed %s", action)
		}
	}
	metrics := map[string]float64{}
	for _, part := range strings.Split(w.Header().Get("Server-Timing"), ", ") {
		parts := strings.Split(part, ";dur=")
		if len(parts) != 2 {
			t.Fatalf("invalid timing %q", part)
		}
		v, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			t.Fatal(err)
		}
		metrics[parts[0]] = v
	}
	if len(metrics) != 5 || metrics["draft_view"] != 40 || control.familyCalls != 1 {
		t.Fatalf("timing=%v counts=%+v", metrics, control)
	}
	for _, name := range []string{"draft_read", "draft_filter", "draft_page", "draft_full", "draft_view"} {
		if _, ok := metrics[name]; !ok {
			t.Fatalf("missing %s: %v", name, metrics)
		}
	}
}

func TestDraftScreenInvalidSnapshotUsesFamilyProjection(t *testing.T) {
	env, service, control, _, _, principal := familyListFixture(t)
	for _, row := range []struct{ id, raw string }{
		{"parent", `{"id":"parent","status":"draft","splitChildren":["child"]}`},
		{"child", `{"id":"child","parentSplit":"parent","status":"closed","body":"large"}`},
	} {
		if _, err := env.DB.Exec(`INSERT INTO drafts(id,json) VALUES($1,$2)`, row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	entries := []DraftFields{{Key: "wrong", Record: Record{"id": "parent", "status": "draft", "splitChildren": []any{"child"}}}}
	views := service.DraftScreenViews(entries, principal, "")
	if len(views) != 1 || control.familyCalls != 1 || control.singleCalls != 0 || control.fallbackCalls != 0 {
		t.Fatalf("views=%v counts=%+v", views, control)
	}
	for _, a := range fleet.Items(views[0]["actions"]) {
		if a == "finish" || a == "discard" {
			t.Fatalf("closed child allowed %s", a)
		}
	}
}

func TestFinishHTTPPreservesNestedFamilyAndLists(t *testing.T) {
	env, _, _, mux, token, _ := familyListFixture(t)
	ctx := context.Background()
	rows := []string{
		`{"id":"00000000-0000-4000-8000-000000000001","project":"owner/app","status":"draft","revision":0,"splitChildren":["00000000-0000-4000-8000-000000000002","00000000-0000-4000-8000-000000000004","00000000-0000-4000-8000-000000000005","00000000-0000-4000-8000-000000000006"],"splitIssues":[{},{},{},{}]}`,
		`{"id":"00000000-0000-4000-8000-000000000002","project":"owner/app","parentSplit":"00000000-0000-4000-8000-000000000001","status":"submitted","revision":3,"splitChildren":["00000000-0000-4000-8000-000000000003"],"issues":[{"number":10,"project":"other/repo"},{"number":11}]}`,
		`{"id":"00000000-0000-4000-8000-000000000003","project":"owner/app","parentSplit":"00000000-0000-4000-8000-000000000002","status":"submitted","revision":4,"issue":{"number":10}}`,
		`{"id":"00000000-0000-4000-8000-000000000004","project":"owner/app","parentSplit":"00000000-0000-4000-8000-000000000001","status":"submitted","revision":2,"issue":{"number":12}}`,
		`{"id":"00000000-0000-4000-8000-000000000005","project":"owner/app","parentSplit":"00000000-0000-4000-8000-000000000001","status":"draft","revision":0}`,
		`{"id":"00000000-0000-4000-8000-000000000006","project":"owner/app","parentSplit":"00000000-0000-4000-8000-000000000001","status":"submitted","revision":1,"finishedWithoutIssue":true}`,
		`{"id":"00000000-0000-4000-8000-000000000007","project":"owner/app","status":"error","revision":2,"messages":[{"role":"user","text":"request"}],"attachments":[{"id":"a"}]}`,
		`{"id":"00000000-0000-4000-8000-000000000008","project":"owner/app","status":"stalled","revision":2,"messages":[{"role":"user","text":"request"}],"attachments":[{"id":"a"}]}`,
	}
	for _, raw := range rows {
		var d map[string]any
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		d["createdAt"] = "2026-11-01T02:00:00Z"
		encoded, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, d["id"], string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	raw := func(id string) string {
		var v string
		if err := env.DB.QueryRowContext(ctx, `SELECT json FROM drafts WHERE id=$1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := map[string]string{}
	for _, id := range []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004", "00000000-0000-4000-8000-000000000006"} {
		before[id] = raw(id)
	}
	request := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://localhost:8787/api/drafts"+path, strings.NewReader(`{"instructionAt":"2026-11-01T02:59:00Z"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	list := familyListRequest(t, mux, token, "")
	for _, item := range list["items"].([]any) {
		d := item.(map[string]any)
		id := d["id"].(string)
		if id != "00000000-0000-4000-8000-000000000001" && id != "00000000-0000-4000-8000-000000000007" && id != "00000000-0000-4000-8000-000000000008" {
			continue
		}
		w := request("GET", "/"+id)
		var detail map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		for _, actions := range []any{d["actions"], detail["actions"]} {
			found := false
			for _, a := range actions.([]any) {
				if a == "finish" {
					found = true
				}
			}
			if !found {
				t.Fatalf("finish missing: %s %v", id, actions)
			}
		}
	}
	for _, id := range []string{"00000000-0000-4000-8000-000000000007", "00000000-0000-4000-8000-000000000008", "00000000-0000-4000-8000-000000000001"} {
		w := request("POST", "/"+id+"/finish")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
		}
	}
	for id, value := range before {
		if raw(id) != value {
			t.Fatalf("changed %s", id)
		}
	}
	var parent map[string]any
	json.Unmarshal([]byte(raw("00000000-0000-4000-8000-000000000001")), &parent)
	issues := parent["issues"].([]any)
	if len(issues) != 3 || issues[0].(map[string]any)["project"] != "other/repo" || issues[1].(map[string]any)["project"] != "owner/app" || issues[2].(map[string]any)["project"] != "owner/app" {
		t.Fatal(parent)
	}
	for _, id := range []string{"00000000-0000-4000-8000-000000000007", "00000000-0000-4000-8000-000000000008", "00000000-0000-4000-8000-000000000005"} {
		var d map[string]any
		json.Unmarshal([]byte(raw(id)), &d)
		if d["status"] != "submitted" {
			t.Fatal(d)
		}
		if (id == "00000000-0000-4000-8000-000000000007" || id == "00000000-0000-4000-8000-000000000008") && (d["revision"] != float64(3) || d["messages"] == nil || d["attachments"] == nil) {
			t.Fatal(d)
		}
	}
	out := familyListRequest(t, mux, token, "?excludeStatus=submitted")
	if len(out["items"].([]any)) != 0 {
		t.Fatal(out)
	}
	all := familyListRequest(t, mux, token, "")
	if len(all["items"].([]any)) != len(rows) {
		t.Fatal(all)
	}
}

func TestFinishHTTPRejectsProtectedRecordsWithoutMutation(t *testing.T) {
	env, service, _, mux, token, _ := familyListFixture(t)
	ctx := context.Background()
	// thinking/auto_ready deliberately allow explicit stop-and-finish. A saved
	// draft whose previous execution is still active must wait for that cleanup.
	cases := []struct {
		status, extra string
		active        bool
	}{{"draft", "", true}, {"submit_unknown", "", false}, {"submitting", "", false}, {"closed", "", false}, {"error", `,"issues":[{"number":42}]`, false}, {"stalled", `,"issue":{"number":42}`, false}}
	for i, tc := range cases {
		id := fmt.Sprintf("10000000-0000-4000-8000-%012d", i)
		raw := fmt.Sprintf(`{"id":%q,"project":"owner/app","status":%q,"createdAt":"2026-11-01T02:00:00Z","revision":1%s}`, id, tc.status, tc.extra)
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, id, raw); err != nil {
			t.Fatal(err)
		}
		service.options.Active = func(active string) bool { return tc.active && active == id }
		req := httptest.NewRequest("POST", "http://localhost:8787/api/drafts/"+id+"/finish", strings.NewReader(`{"instructionAt":"2026-11-01T02:59:00Z"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 409 {
			t.Fatalf("%s: %d %s", tc.status, w.Code, w.Body.String())
		}
		var after string
		if err := env.DB.QueryRowContext(ctx, `SELECT json FROM drafts WHERE id=$1`, id).Scan(&after); err != nil || after != raw {
			t.Fatalf("%s changed: %s %v", tc.status, after, err)
		}
		list := familyListRequest(t, mux, token, "")
		found := false
		for _, rawItem := range list["items"].([]any) {
			item := rawItem.(map[string]any)
			if item["id"] != id {
				continue
			}
			found = true
			req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts/"+id, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			detailResponse := httptest.NewRecorder()
			mux.ServeHTTP(detailResponse, req)
			var detail map[string]any
			if detailResponse.Code != 200 || json.Unmarshal(detailResponse.Body.Bytes(), &detail) != nil {
				t.Fatalf("detail: %d %s", detailResponse.Code, detailResponse.Body.String())
			}
			for _, actions := range []any{item["actions"], detail["actions"]} {
				for _, action := range fleet.Items(actions) {
					if action == "finish" {
						t.Fatalf("protected %s offers finish: %v", tc.status, actions)
					}
				}
			}
		}
		if !found {
			t.Fatalf("protected record %s absent from list", id)
		}
	}
	id := "20000000-0000-4000-8000-000000000001"
	raw := `{"id":"20000000-0000-4000-8000-000000000001","project":"owner/app","status":"error","createdAt":"2026-11-01T02:00:00Z","revision":1}`
	if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, id, raw); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ bearer, body string }{{token, `{"instructionAt":"2020-01-01T00:00:00Z"}`}, {"", `{"instructionAt":"2026-11-01T02:59:00Z"}`}} {
		req := httptest.NewRequest("POST", "http://localhost:8787/api/drafts/"+id+"/finish", strings.NewReader(tc.body))
		if tc.bearer != "" {
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code < 400 {
			t.Fatalf("accepted: %d", w.Code)
		}
		var after string
		if err := env.DB.QueryRowContext(ctx, `SELECT json FROM drafts WHERE id=$1`, id).Scan(&after); err != nil || after != raw {
			t.Fatalf("changed: %s %v", after, err)
		}
	}

}

func TestFinishChildRejectsAutoReadyParentPreservesFamily(t *testing.T) {
	env, _, _, mux, token, _ := familyListFixture(t)
	parent := "30000000-0000-4000-8000-000000000001"
	child := "30000000-0000-4000-8000-000000000002"
	sibling := "30000000-0000-4000-8000-000000000003"
	before := map[string]string{
		parent:  fmt.Sprintf(`{"id":%q,"project":"owner/app","createdAt":"2026-11-01T02:00:00Z","status":"auto_ready","splitChildren":[%q,%q],"splitIssues":[{},{}],"revision":2}`, parent, child, sibling),
		child:   fmt.Sprintf(`{"id":%q,"project":"owner/app","createdAt":"2026-11-01T02:00:00Z","status":"draft","parentSplit":%q,"revision":1}`, child, parent),
		sibling: fmt.Sprintf(`{"id":%q,"project":"owner/app","createdAt":"2026-11-01T02:00:00Z","status":"submitted","parentSplit":%q,"issue":{"number":42},"revision":3}`, sibling, parent),
	}
	for id, raw := range before {
		if _, err := env.DB.Exec(`INSERT INTO drafts(id,json) VALUES($1,$2)`, id, raw); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("POST", "http://localhost:8787/api/drafts/"+child+"/finish", strings.NewReader(`{"instructionAt":"2026-11-01T02:59:00Z"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, req)
	if response.Code != 409 {
		t.Fatalf("auto-ready parent accepted child finish: %d %s", response.Code, response.Body.String())
	}
	for id, raw := range before {
		var after string
		if err := env.DB.QueryRow(`SELECT json FROM drafts WHERE id=$1`, id).Scan(&after); err != nil || after != raw {
			t.Fatalf("family record %s changed: %s %v", id, after, err)
		}
	}
	var ready map[string]any
	if err := json.Unmarshal([]byte(before[parent]), &ready); err != nil {
		t.Fatal(err)
	}
	ready["status"] = "draft"
	raw, _ := json.Marshal(ready)
	if _, err := env.DB.Exec(`UPDATE drafts SET json=$2 WHERE id=$1`, parent, string(raw)); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("POST", "http://localhost:8787/api/drafts/"+child+"/finish", strings.NewReader(`{"instructionAt":"2026-11-01T02:59:00Z"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, req)
	if response.Code != 200 {
		t.Fatalf("same family with draft parent should finish: %d %s", response.Code, response.Body.String())
	}

}
