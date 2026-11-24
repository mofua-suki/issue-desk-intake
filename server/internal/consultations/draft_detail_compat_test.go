package consultations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
)

// The detail branch of draft-routes.go at 917ef4ed8241264c08859c49c32f0a7985a8d382,
// including its authentication, access, full-list failure and first-match behavior.
func legacyDetail(s *Service, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.options.Auth.Authenticate(r)
		if err != nil {
			contract.WriteProblem(w, err)
			return
		}
		if err = auth.Authorize(p, auth.Policy{Screen: true, CLI: true, PC: true}); err != nil {
			contract.WriteProblem(w, err)
			return
		}
		ac, err := s.options.Auth.Context(r)
		if err != nil {
			contract.WriteProblem(w, err)
			return
		}
		d, readErr := s.readDraft(r.Context(), id, p)
		if readErr != nil {
			contract.WriteProblem(w, problem(readErr))
			return
		}
		if d["parentSplit"] == nil && d["splitChildren"] != nil {
			if rows, e := s.options.Records.List(r.Context(), "drafts"); e == nil {
				for _, candidate := range s.FamilyProgress(r.Context(), rows) {
					if candidate["id"] == d["id"] {
						d = candidate
						break
					}
				}
			}
		}
		respond(w, 200, s.DraftView(d, p, ac.Origin))
	}
}

func TestDraftDetailLegacyCompatibility(t *testing.T) {
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
	readDB := sql.OpenDB(routeReadConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
	defer readDB.Close()
	s, db, _ := fixture(t)
	token := stopToken(t, db, true)
	plain := "idk_" + strings.Repeat("b", 43)
	if _, err := db.AddPCToken(ctx, store.PCTokenRecord{ID: "plain", Name: "plain", Hash: auth.Hash(plain), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	const parent = "11111111-1111-4111-8111-111111111111"
	parentJSON := `{"id":"` + parent + `","project":"owner/app","status":"draft","body":"parent","splitChildren":["child","missing"],"splitIssues":[{},{},{}]}`
	childJSON := `{"id":"child","project":"owner/app","status":"submitted","parentSplit":"` + parent + `","issue":{"number":7}}`
	type testCase struct {
		name, parent, child, extraKey, extra                                         string
		job                                                                          string
		jobsFail, issueFail, selectedFail, projectionFail, listFail, denied, missing bool
	}
	cases := []testCase{
		{name: "merged", job: "merged"}, {name: "failed", job: "failed"}, {name: "closed-job", job: "closed"},
		{name: "unfiled", child: `{"id":"child","project":"owner/app","status":"draft","parentSplit":"` + parent + `"}`},
		{name: "closed-child", child: `{"id":"child","project":"owner/app","status":"closed","parentSplit":"` + parent + `"}`},
		{name: "no-job-or-issue", job: "none", issueFail: true}, {name: "issue-closed", job: "none"},
		{name: "missing-child", child: "DELETE"}, {name: "jobs-failure", jobsFail: true},
		{name: "selected-failure", selectedFail: true}, {name: "projection-failure", projectionFail: true}, {name: "all-reads-fail", projectionFail: true, listFail: true},
		{name: "denied", denied: true}, {name: "absent", missing: true},
		{name: "id-missing", extraKey: "extra", extra: `{"body":"absent id"}`},
		{name: "id-null", extraKey: "extra", extra: `{"id":null}`},
		{name: "id-number", extraKey: "extra", extra: `{"id":12}`},
		{name: "id-duplicate", extraKey: "zz", extra: `{"id":"` + parent + `","body":"duplicate"}`},
		{name: "key-mismatch", extraKey: "extra", extra: `{"id":"child","body":"collision"}`},
		{name: "json-duplicate-id", extraKey: "extra", extra: `{"id":"bad","id":"extra"}`},
		{name: "nul-and-surrogate", extraKey: "extra", extra: `{"id":"extra","body":"\u0000\ud800"}`},
		{name: "child-id-nonstring", parent: `{"id":"` + parent + `","project":"owner/app","splitChildren":[null,7],"splitIssues":[{}]}`},
		{name: "parent-id-missing", parent: `{"project":"owner/app","splitChildren":["child"]}`},
		{name: "parent-id-null", parent: `{"id":null,"project":"owner/app","splitChildren":["child"]}`},
		{name: "parent-id-number", parent: `{"id":7,"project":"owner/app","splitChildren":["child"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := env.DB.ExecContext(ctx, `DELETE FROM drafts`); err != nil {
				t.Fatal(err)
			}
			insert := func(key, raw string) {
				t.Helper()
				if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, key, raw); err != nil {
					t.Fatal(err)
				}
			}
			p := tc.parent
			if p == "" {
				p = parentJSON
			}
			if !tc.missing {
				insert(parent, p)
			}
			c := tc.child
			if c == "" {
				c = childJSON
			}
			if c != "DELETE" {
				insert("child", c)
			}
			if tc.extra != "" {
				insert(tc.extraKey, tc.extra)
			}
			s.options.Records = &Records{DB: readDB}
			s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
				if tc.jobsFail {
					return nil, errors.New("jobs")
				}
				if tc.job == "none" {
					return nil, nil
				}
				status := tc.job
				if status == "" {
					status = "merged"
				}
				return []FamilyJob{{Project: "owner/app", Number: 7, Status: status, Reason: "failure", Desk: 17}}, nil
			}
			s.options.FamilyIssues = func(context.Context, string) (map[string]string, error) {
				if tc.issueFail {
					return nil, errors.New("issues")
				}
				return map[string]string{"owner/app#7": "closed"}, nil
			}
			req := func() *http.Request {
				r := httptest.NewRequest("GET", "http://localhost:8787/api/drafts/"+parent, nil)
				bearer := token
				if tc.denied {
					bearer = plain
				}
				r.Header.Set("Authorization", "Bearer "+bearer)
				return r
			}
			*control = routeReadControl{failList: tc.listFail}
			before := httptest.NewRecorder()
			legacyDetail(s, parent)(before, req())
			*control = routeReadControl{fail: tc.selectedFail, failProjection: tc.projectionFail, failList: tc.listFail}
			after := httptest.NewRecorder()
			mux := http.NewServeMux()
			s.Register(mux)
			mux.ServeHTTP(after, req())
			if tc.selectedFail && (control.fullCalls != 1 || control.listCalls != 1) {
				t.Fatalf("selected failure did not fall back: %+v", control)
			}
			if tc.projectionFail && (control.projectionCalls != 1 || control.listCalls != 1) {
				t.Fatalf("projection failure did not fall back: %+v", control)
			}
			if (tc.name == "id-missing" || tc.name == "id-null" || tc.name == "id-number" || tc.name == "id-duplicate" || tc.name == "key-mismatch") && (control.listCalls != 1 || control.fullCalls != 0) {
				t.Fatalf("abnormal IDs did not use full list: %+v", control)
			}
			if tc.name == "merged" && (control.listCalls != 0 || control.fullCalls != 1 || control.fullRows != 2) {
				t.Fatalf("normal family did not use selected reads: %+v", control)
			}
			var a, b any
			if err := json.Unmarshal(before.Body.Bytes(), &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(after.Body.Bytes(), &b); err != nil {
				t.Fatal(err)
			}
			if before.Code != after.Code || !reflect.DeepEqual(a, b) {
				t.Fatalf("legacy %d %s; actual %d %s", before.Code, before.Body, after.Code, after.Body)
			}
			if tc.denied && after.Code != http.StatusNotFound {
				t.Fatalf("expected access denial: %d", after.Code)
			}
			if tc.missing && after.Code != http.StatusNotFound {
				t.Fatalf("expected absent: %d", after.Code)
			}
		})
	}
}

// Real PostgreSQL, fixed records and deterministic external results; allocations
// are process-wide TotalAlloc deltas, so they are comparative measurements.
func TestDraftDetailLegacyMeasurement(t *testing.T) {
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
	readDB := sql.OpenDB(routeReadConnector{appsql.NewPostgresBindingConnector(stdlib.GetConnector(*cfg)), control})
	defer readDB.Close()
	s, db, _ := fixture(t)
	s.options.Records = &Records{DB: readDB}
	token := stopToken(t, db, true)
	s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
		return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
	}
	const parent = "11111111-1111-4111-8111-111111111111"
	insert := func(id, raw string) {
		t.Helper()
		if _, err := env.DB.ExecContext(ctx, `INSERT INTO drafts(id,json) VALUES($1,$2)`, id, raw); err != nil {
			t.Fatal(err)
		}
	}
	insert(parent, `{"id":"`+parent+`","project":"owner/app","splitChildren":["child"],"splitIssues":[{}],"body":"parent"}`)
	insert("child", `{"id":"child","project":"owner/app","parentSplit":"`+parent+`","issue":{"number":7}}`)
	for i := 0; i < 80; i++ {
		id := fmt.Sprintf("other-%03d", i)
		insert(id, `{"id":"`+id+`","project":"owner/app","body":"`+strings.Repeat("x", 16384)+`"}`)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	old := legacyDetail(s, parent)
	run := func(h http.Handler) (time.Duration, readCounts, uint64, []byte) {
		t.Helper()
		*control = routeReadControl{}
		runtime.GC()
		var m1, m2 runtime.MemStats
		runtime.ReadMemStats(&m1)
		start := time.Now()
		var body []byte
		for i := 0; i < 10; i++ {
			r := httptest.NewRequest("GET", "http://localhost:8787/api/drafts/"+parent, nil)
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			body = append([]byte(nil), w.Body.Bytes()...)
		}
		elapsed := time.Since(start)
		runtime.ReadMemStats(&m2)
		return elapsed, control.counts, m2.TotalAlloc - m1.TotalAlloc, body
	}
	// Warm both paths; include cached projection in the steady-state measurement.
	run(old)
	run(mux)
	oldTime, oldReads, oldAlloc, oldBody := run(old)
	newTime, newReads, newAlloc, newBody := run(mux)
	if !reflect.DeepEqual(oldBody, newBody) {
		t.Fatalf("responses differ: %s %s", oldBody, newBody)
	}
	t.Logf("10 requests, 80 unrelated x 16384 bytes: legacy time=%s reads=%d rows=%d bytes=%d alloc=%d; selected time=%s reads=%d rows=%d bytes=%d alloc=%d", oldTime, oldReads.calls, oldReads.rows, oldReads.bytes, oldAlloc, newTime, newReads.calls, newReads.rows, newReads.bytes, newAlloc)
	if newReads.bytes >= oldReads.bytes {
		t.Fatalf("selected bytes not reduced: legacy=%d selected=%d", oldReads.bytes, newReads.bytes)
	}
}
