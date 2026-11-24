package consultations

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/store"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/pgtest"
)

// Advance a controlled clock in each stage to verify the response metrics independently of system load.
func TestDraftFullTimingStages(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, raw string }{
		{"parent", `{"id":"parent","project":"owner/app","status":"draft","body":"full","familyProgress":{"stale":true},"splitChildren":["child"],"splitIssues":[{}]}`},
		{"child", `{"id":"child","project":"owner/app","status":"submitted","parentSplit":"parent","issue":{"number":7}}`},
	} {
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	control := &routeReadControl{}
	db := sql.OpenDB(routeReadConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
	defer db.Close()
	s, authDB, _ := fixture(t)
	s.options.Records = &Records{DB: db}
	token := stopToken(t, authDB, true)
	mux := http.NewServeMux()
	s.Register(mux)
	for _, stage := range []string{"filter", "full", "view"} {
		t.Run(stage, func(t *testing.T) {
			const delay = 120 * time.Millisecond
			clock := time.Unix(0, 0)
			s.timingNow = func() time.Time { return clock }
			*control = routeReadControl{}
			s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
				if stage == "filter" {
					clock = clock.Add(delay)
				}
				return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
			}
			var once sync.Once
			s.options.Active = func(string) bool {
				if stage == "view" {
					once.Do(func() { clock = clock.Add(delay) })
				}
				return false
			}
			if stage == "full" {
				control.beforeFull = func() { clock = clock.Add(delay) }
			}
			req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts?status=draft&limit=1", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			var out struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || len(out.Items) != 1 {
				t.Fatalf("%d %s %v", w.Code, w.Body.String(), err)
			}
			progress, ok := out.Items[0]["familyProgress"].(map[string]any)
			if !ok || progress["completed"] != float64(1) || out.Items[0]["body"] != "full" || out.Items[0]["actions"] == nil {
				t.Fatal(out.Items)
			}
			metrics := map[string]float64{}
			for _, part := range strings.Split(w.Header().Get("Server-Timing"), ", ") {
				pair := strings.Split(part, ";dur=")
				if len(pair) == 2 {
					metrics[pair[0]], _ = strconv.ParseFloat(pair[1], 64)
				}
			}
			name := "draft_" + stage
			if metrics[name] != float64(delay.Milliseconds()) {
				t.Fatalf("%s missing controlled delay: %v", name, metrics)
			}
			for _, other := range []string{"draft_read", "draft_filter", "draft_page", "draft_full", "draft_view"} {
				if _, ok := metrics[other]; !ok || (other != name && metrics[other] != 0) {
					t.Fatalf("%s delay attributed to %s: %v", name, other, metrics)
				}
			}
			t.Logf("%s: %v", stage, metrics)
		})
	}
}

// BenchmarkDraftLargePage is deliberately separate from the normal test run.
// It exercises the HTTP endpoint and compares its result with a reconstruction
// of the previous all-records, view-before-pagination route.
func BenchmarkDraftLargePage(b *testing.B) {
	ctx := context.Background()
	preparation := time.Now()
	env := pgtest.NewIsolated(b)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		b.Fatal(err)
	}
	_, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json)
 SELECT format('key-%s',lpad(i::text,5,'0')),
 json_build_object('id',format('json-%s',lpad(i::text,5,'0')),'project','owner/app',
 'status',CASE WHEN i%2=0 THEN 'draft' ELSE 'finished' END,'createdAt','same',
 'body',repeat('b',8192),'savedMaterials',repeat('s',8192),
 'messages',json_build_array(json_build_object('role','user','text',repeat('m',4096)),json_build_object('role','assistant','text',repeat('m',4096)),json_build_object('role','user','text',repeat('m',4096)),json_build_object('role','assistant','text',repeat('m',4096))))::text
 FROM generate_series(0,6701) i`)
	if err != nil {
		b.Fatal(err)
	}
	_, err = env.DB.ExecContext(ctx, `UPDATE drafts SET json=(json::jsonb || CASE id
	 WHEN 'key-06700' THEN '{"id":"key-06700","splitChildren":["key-06699"],"splitIssues":[{}],"familyProgress":{"stale":true}}'::jsonb
	 ELSE '{"id":"key-06699","parentSplit":"key-06700","issue":{"number":7}}'::jsonb END)::text
	 WHERE id IN ('key-06700','key-06699')`)
	if err != nil {
		b.Fatal(err)
	}
	_, err = env.DB.ExecContext(ctx, `UPDATE drafts SET json=jsonb_set(json::jsonb,'{messages,0,images}','[{"id":"33333333-3333-4333-8333-333333333333"}]'::jsonb,true)::text WHERE id='key-06700'`)
	if err != nil {
		b.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		b.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	counts := &pageReadLog{}
	readDB := sql.OpenDB(pageCountConnector{stdlib.GetConnector(*cfg), counts})
	defer readDB.Close()
	authStore, err := store.OpenApplicationPostgres(ctx, env.DSN, env.Schema)
	if err != nil {
		b.Fatal(err)
	}
	defer authStore.Close()
	a, err := auth.New(auth.Config{Port: 8787}, &store.AuthStore{SQLite: authStore})
	if err != nil {
		b.Fatal(err)
	}
	key := "test-large-page-cli-key"
	if err := authStore.AddCLIKey(ctx, auth.Hash(key), time.Now().Add(time.Hour)); err != nil {
		b.Fatal(err)
	}
	s, err := New(Options{Records: &Records{DB: readDB}, Auth: a, Project: func(string) error { return nil }})
	if err != nil {
		b.Fatal(err)
	}
	s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
		return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
	}
	mux := http.NewServeMux()
	s.Register(mux)
	call := func(limit int, legacy bool) (time.Duration, []byte, pageReadLog) {
		if err := authStore.AddCLIKey(ctx, auth.Hash(key), time.Now().Add(time.Hour)); err != nil {
			b.Fatal(err)
		}
		*counts = pageReadLog{}
		req := httptest.NewRequest("GET", fmt.Sprintf("http://localhost:8787/api/drafts?status=draft&limit=%d", limit), nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Desk-Key", key)
		w := httptest.NewRecorder()
		start := time.Now()
		if !legacy {
			mux.ServeHTTP(w, req)
		} else {
			principal, failure := a.Authenticate(req)
			if failure != nil {
				b.Fatal(failure)
			}
			if failure := auth.Authorize(principal, auth.Policy{Screen: true, CLI: true, PC: true, RequireDelegation: true}); failure != nil {
				b.Fatal(failure)
			}
			ac, failure := a.Context(req)
			if failure != nil {
				b.Fatal(failure)
			}
			list, err := s.options.Records.List(ctx, "drafts")
			if err != nil {
				b.Fatal(err)
			}
			list = s.FamilyProgress(ctx, list)
			views := make([]Record, 0, 3351)
			for _, d := range list {
				if s.draftAccess(d, principal) == nil && d["status"] == "draft" {
					views = append(views, s.DraftView(d, principal, ac.Origin))
				}
			}
			respond(w, 200, contract.Paginate(views, contract.Pagination{Page: 1, Limit: limit}))
		}
		elapsed := time.Since(start)
		if w.Code != 200 {
			b.Fatalf("status=%d %s", w.Code, w.Body.String())
		}
		var page struct {
			Items   []map[string]any `json:"items"`
			Total   int              `json:"total"`
			HasMore bool             `json:"hasMore"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != limit || page.Total != 3351 || !page.HasMore {
			b.Fatalf("page limit=%d total=%d err=%v", limit, page.Total, err)
		}
		for i, item := range page.Items {
			wantID := fmt.Sprintf("json-%05d", 6700-2*i)
			if i == 0 {
				wantID = "key-06700"
			}
			body, bodyOK := item["body"].(string)
			material, matOK := item["savedMaterials"].(string)
			messages, msgOK := item["messages"].([]any)
			if item["id"] != wantID || !bodyOK || len(body) != 8192 || !matOK || len(material) != 8192 || !msgOK || len(messages) != 4 || item["actions"] == nil {
				b.Fatalf("limit=%d item=%d id=%v, body=%d materials=%d messages=%d", limit, i, item["id"], len(body), len(material), len(messages))
			}
			if i == 0 {
				progress, ok := item["familyProgress"].(map[string]any)
				images, imgOK := messages[0].(map[string]any)["images"].([]any)
				if !ok || progress["completed"] != float64(1) || !imgOK || len(images) != 1 || !strings.Contains(fmt.Sprint(images[0].(map[string]any)["url"]), "/api/images/") {
					b.Fatalf("parent progress=%v images=%v", progress, images)
				}
			} else if _, exists := item["familyProgress"]; exists {
				b.Fatalf("unexpected family progress for %s", wantID)
			}
		}
		if !legacy && (counts.full.calls != 1 || counts.full.rows != limit || counts.fallback.calls != 0 || counts.projectionFallback.rows != 0) {
			b.Fatalf("unexpected read volume: %+v", counts)
		}
		return elapsed, w.Body.Bytes(), *counts
	}
	b.Logf("preparation=%s; dataset=6702 active=3351 body=8192 messages=4x4096 savedMaterials=8192", time.Since(preparation))
	for n := 0; n < b.N; n++ {
		for _, limit := range []int{1, 100, 200} {
			var before, after []time.Duration
			for rep := 0; rep < 3; rep++ {
				if limit == 1 {
					s.options.Records = &Records{DB: readDB}
				}
				old, oldBody, oldCounts := call(limit, true)
				cold, newBody, coldCounts := call(limit, false)
				if !bytes.Equal(oldBody, newBody) {
					b.Fatalf("legacy and current response differ for limit %d", limit)
				}
				warm, warmBody, warmCounts := call(limit, false)
				if !bytes.Equal(newBody, warmBody) {
					b.Fatalf("warm response differs for limit %d", limit)
				}
				before = append(before, old)
				after = append(after, warm)
				b.Logf("limit=%d repetition=%d old=%s old_reads=%+v current_first=%s first_reads=%+v warm=%s warm_reads=%+v", limit, rep+1, old, oldCounts, cold, coldCounts, warm, warmCounts)
			}
			sort.Slice(before, func(i, j int) bool { return before[i] < before[j] })
			sort.Slice(after, func(i, j int) bool { return after[i] < after[j] })
			b.Logf("limit=%d median_old=%s median_warm=%s warm_ratio=%.3f target=0.2", limit, before[1], after[1], float64(after[1])/float64(before[1]))
		}
	}
}

func TestDraftFullTimingMutationAndRecheck(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	parent := `{"id":"parent","project":"owner/app","status":"draft","body":"before","splitChildren":["child"],"splitIssues":[{}]}`
	for _, row := range []struct{ id, raw string }{{"parent", parent}, {"child", `{"id":"child","project":"owner/app","status":"submitted","parentSplit":"parent","issue":{"number":7}}`}} {
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, row.id, row.raw); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	control := &routeReadControl{}
	db := sql.OpenDB(routeReadConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
	defer db.Close()
	s, authDB, _ := fixture(t)
	s.options.Records = &Records{DB: db}
	s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
		return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
	}
	token := stopToken(t, authDB, true)
	mux := http.NewServeMux()
	s.Register(mux)
	for _, tc := range []struct {
		name, updated string
		visible       bool
	}{
		{"decode_and_progress", `{"id":"parent","project":"owner/app","status":"draft","body":"after","splitChildren":["child"],"familyProgress":{"stale":true}}`, true},
		{"filter_recheck", `{"id":"parent","project":"owner/app","status":"closed","body":"after"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json=$1 WHERE id='parent'`, parent); err != nil {
				t.Fatal(err)
			}
			mutated := false
			clock := time.Unix(0, 0)
			s.timingNow = func() time.Time { return clock }
			*control = routeReadControl{beforeFull: func() {
				if _, err := env.DB.ExecContext(ctx, `UPDATE drafts SET json=$1 WHERE id='parent'`, tc.updated); err != nil {
					t.Fatal(err)
				}
				mutated = true
				clock = clock.Add(120 * time.Millisecond)
			}}
			req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts?status=draft&limit=1", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			var out struct {
				Items []map[string]any `json:"items"`
				Total int              `json:"total"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 || !mutated || out.Total != 1 {
				t.Fatalf("status=%d body=%s mutated=%v err=%v", w.Code, w.Body.String(), mutated, err)
			}
			if tc.visible {
				if len(out.Items) != 1 || out.Items[0]["body"] != "after" {
					t.Fatal(out.Items)
				}
				progress, ok := out.Items[0]["familyProgress"].(map[string]any)
				if !ok || progress["completed"] != float64(1) || progress["stale"] != nil {
					t.Fatalf("selection progress not restored: %v", out.Items)
				}
			} else if len(out.Items) != 0 {
				t.Fatalf("changed status was not rechecked: %v", out.Items)
			}
			metrics := map[string]float64{}
			for _, part := range strings.Split(w.Header().Get("Server-Timing"), ", ") {
				pair := strings.Split(part, ";dur=")
				if len(pair) == 2 {
					metrics[pair[0]], _ = strconv.ParseFloat(pair[1], 64)
				}
			}
			if metrics["draft_full"] != 120 {
				t.Fatalf("controlled delay not included in draft_full: %v", metrics)
			}
			t.Logf("%s full metric=%gms; selection total=%d returned=%d", tc.name, metrics["draft_full"], out.Total, len(out.Items))
		})
	}
}

func TestDraftProjectionDelayOutsideFullTiming(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	if _, err := OpenRecords(ctx, env.DB); err != nil {
		t.Fatal(err)
	}
	if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES('a','{"id":"a","project":"owner/app","status":"draft","body":"complete"}')`); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = env.Schema
	clock := time.Unix(0, 0)
	var once sync.Once
	log := &pageReadLog{beforeQuery: func(kind int) {
		if kind == 1 {
			once.Do(func() { clock = clock.Add(120 * time.Millisecond) })
		}
	}}
	db := sql.OpenDB(pageCountConnector{stdlib.GetConnector(*cfg), log})
	defer db.Close()
	s, authDB, _ := fixture(t)
	s.options.Records = &Records{DB: db}
	s.timingNow = func() time.Time { return clock }
	token := stopToken(t, authDB, true)
	mux := http.NewServeMux()
	s.Register(mux)
	req := httptest.NewRequest("GET", "http://localhost:8787/api/drafts?limit=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || len(page.Items) != 1 || page.Items[0]["body"] != "complete" {
		t.Fatalf("status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
	metrics := map[string]float64{}
	for _, part := range strings.Split(w.Header().Get("Server-Timing"), ", ") {
		pair := strings.Split(part, ";dur=")
		if len(pair) == 2 {
			metrics[pair[0]], _ = strconv.ParseFloat(pair[1], 64)
		}
	}
	if log.projection.calls != 1 || metrics["draft_read"] != 120 || metrics["draft_full"] != 0 {
		t.Fatalf("projection delay timing=%v reads=%+v", metrics, log)
	}
	t.Logf("projection delay: %v", metrics)
}
