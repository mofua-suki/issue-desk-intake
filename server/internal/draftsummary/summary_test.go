package draftsummary_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/draftsummary"
	"issue-desk-intake/server/internal/pgtest"
)

// Independent reference for the current DraftReadScreen projection.
// Keep this independent of both the installed function and production readers.
const draftSummaryReference = `SELECT CASE WHEN position('\u0000' in json) > 0 THEN json WHEN json_typeof(json::json) = 'object' THEN (SELECT COALESCE(json_object_agg(key,value), '{}'::json) FROM (SELECT key,value FROM json_each(json::json) WHERE key IN ('id','owner','status','project','unassigned','title','shownTitle','createdAt','updatedAt','priority','retryAt','waitStartedAt','turnStopped','queued','continuationLimited','investigationNotice','investigations','priorityReviewRound','usageReviewRound','inventoryRound','parentSplit','questions','issue','issues','projectHandoff','handoffRow','origin','followup','priorityChangeable','finishingTurn','splitChildren','finishedWithoutIssue','ready','kind','consultationChildren','basisConflicts','attempted','_stopped') UNION ALL SELECT 'proposal', CASE WHEN json_typeof(json::json->'proposal') = 'null' THEN 'null'::json ELSE json_build_object('kind', json::json->'proposal'->'kind') END WHERE jsonb_exists(json::jsonb, 'proposal') UNION ALL SELECT key,value::json FROM jsonb_each('{}'::jsonb || jsonb_build_object('messages', (
			SELECT COALESCE(jsonb_agg(m), '[]'::jsonb) FROM (
				(SELECT jsonb_build_object('role','user','text', CASE WHEN jsonb_typeof(value->'text') = 'string' THEN left(value->>'text',60) ELSE '' END) AS m, 1 AS n
				 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(json::jsonb->'messages') = 'array' THEN json::jsonb->'messages' ELSE '[]'::jsonb END) WITH ORDINALITY AS u(value, ord)
				 WHERE value->>'role' = 'user' ORDER BY ord LIMIT 1)
				UNION ALL SELECT '{"role":"proxy"}'::jsonb, 2 WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(json::jsonb->'messages') = 'array' THEN json::jsonb->'messages' ELSE '[]'::jsonb END) v WHERE v->>'role' = 'proxy')
				UNION ALL SELECT '{"role":"image-marker","images":[{"id":"00000000-0000-0000-0000-000000000000"}]}'::jsonb, 3 WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(json::jsonb->'messages') = 'array' THEN json::jsonb->'messages' ELSE '[]'::jsonb END) v CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(v->'images') = 'array' THEN v->'images' ELSE '[]'::jsonb END) img WHERE img->>'id' ~ '^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$')
				ORDER BY n) summary), 'body', CASE WHEN jsonb_typeof(json::jsonb->'body') = 'string' AND position('<!-- issue-desk:followup -->' in json::jsonb->>'body') > 0 THEN '<!-- issue-desk:followup -->' ELSE '' END,
			'usageMeasurementHold', CASE WHEN json::jsonb->'usageMeasurementHold' IS NOT NULL AND json::jsonb->'usageMeasurementHold' <> 'null'::jsonb THEN 'true'::jsonb ELSE 'null'::jsonb END,
			'consultationBudget', jsonb_build_object('stopped', CASE WHEN jsonb_typeof(json::jsonb->'consultationBudget'->'stopped') = 'object' THEN '{}'::jsonb ELSE json::jsonb->'consultationBudget'->'stopped' END,
                'pendingCall', CASE WHEN json::jsonb->'consultationBudget'->'pendingCall' IS NOT NULL AND json::jsonb->'consultationBudget'->'pendingCall' <> 'null'::jsonb THEN 'true'::jsonb ELSE 'null'::jsonb END,
                'unknownCalls', json::jsonb->'consultationBudget'->'unknownCalls'))|| CASE WHEN jsonb_exists(json::jsonb, 'splitIssues') THEN jsonb_build_object('splitIssues', CASE WHEN jsonb_typeof(json::jsonb->'splitIssues') = 'array' THEN (SELECT COALESCE(jsonb_agg(CASE WHEN jsonb_typeof(v->'project') = 'string' THEN jsonb_build_object('project',v->'project') ELSE '{}'::jsonb END ORDER BY ord), '[]'::jsonb) FROM jsonb_array_elements(json::jsonb->'splitIssues') WITH ORDINALITY AS entries(v,ord)) WHEN json::jsonb->'splitIssues' = 'null'::jsonb THEN 'null'::jsonb ELSE 'true'::jsonb END) ELSE '{}'::jsonb END)) AS fields)::text ELSE json END FROM (SELECT $1::text AS json) AS draft`

type summaryFixture struct {
	id, raw     string
	nullSummary bool
}

func summaryFixtures() []summaryFixture {
	return []summaryFixture{
		{"normal", `{"id":"normal","owner":"u","status":"draft","project":"p","unassigned":false,"title":"title","shownTitle":"shown","createdAt":"a","updatedAt":"b","priority":1,"retryAt":"r","waitStartedAt":"w","turnStopped":true,"queued":false,"continuationLimited":true,"investigationNotice":{},"investigations":[],"priorityReviewRound":2,"usageReviewRound":3,"inventoryRound":4,"parentSplit":"p","questions":[],"issue":{"number":1},"issues":[],"projectHandoff":{},"handoffRow":true,"origin":"o","followup":true,"priorityChangeable":true,"finishingTurn":true,"splitChildren":["c"],"finishedWithoutIssue":false,"ready":true,"kind":"consultation","consultationChildren":[],"basisConflicts":[],"attempted":true,"_stopped":{},"body":"discard","savedMaterials":"discard"}`, false},
		{"messages", `{"messages":[{"role":"assistant","text":"ignore"},{"role":"user","text":"` + strings.Repeat("😀あ", 40) + `","images":[{"id":"ABCDEF00-0000-0000-0000-000000000000"}]},{"role":"user","text":"second"},{"role":"proxy","images":[{"id":"01234567-89ab-cdef-0123-456789abcdef"}]}]}`, false},
		{"invalid-images", `{"messages":[{"role":"user","text":42,"images":[{"id":"bad"},{"id":"ABCDEF00-0000-0000-0000-000000000000"}]},{"role":"image-marker","images":{}}]}`, false},
		{"typed-messages", `{"messages":{},"body":42}`, false},
		{"followup", `{"body":"before <!-- issue-desk:followup --> after","consultationBudget":{"stopped":{"calls":9007199254740993},"pendingCall":{},"unknownCalls":3},"usageMeasurementHold":{},"proposal":{"kind":1e3,"body":"omit"}}`, false},
		{"proposal-null", `{"proposal":null,"consultationBudget":{"stopped":null}}`, false},
		{"proposal-scalar", `{"proposal":42,"consultationBudget":{"stopped":true}}`, false},
		{"split-array", `{"splitIssues":[{"project":"p","body":"omit"},{"project":42},null,{},"x"],"proposal":{"kind":9007199254740993}}`, false},
		{"split-empty", `{"splitIssues":[]}`, false},
		{"split-null", `{"splitIssues":null}`, false},
		{"split-other", `{"splitIssues":{"project":"p"}}`, false},
		{"missing-id", `{"status":"draft","project":1e3,"priority":9007199254740993}`, false},
		{"duplicate", `{"id":"first","id":"last","project":1e3,"project":9007199254740993,"proposal":{"kind":1e3,"kind":9007199254740993}}`, false},
		{"nul", `{"title":"a\u0000b"}`, true},
		{"literal-nul-escape", `{"title":"a\\u0000b"}`, true},
		{"surrogate", `{"title":"\ud800"}`, true},
		{"broken", `{`, true},
		{"null", `null`, true},
		{"array", `[]`, true},
		{"number", `42`, true},
	}
}

type summaryExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func summaryExec(t *testing.T, db summaryExecer, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func assertSummaryFixture(t *testing.T, env pgtest.Env, f summaryFixture) {
	t.Helper()
	var original string
	var got, generated sql.NullString
	if err := env.DB.QueryRow(`SELECT json,screen_summary,`+env.Schema+`.drafts_screen_summary(json) FROM drafts WHERE id=$1`, f.id).Scan(&original, &got, &generated); err != nil {
		t.Fatal(err)
	}
	if got != generated {
		t.Fatalf("%s: saved %#v differs from current function %#v", f.id, got, generated)
	}
	if original != f.raw {
		t.Fatalf("%s: original JSON changed", f.id)
	}
	var want string
	err := env.DB.QueryRow(draftSummaryReference, f.raw).Scan(&want)
	if f.nullSummary {
		if err == nil && want != f.raw {
			t.Fatalf("%s: baseline unexpectedly projected: %s", f.id, want)
		}
		if got.Valid {
			t.Fatalf("%s: expected SQL NULL, got %s", f.id, got.String)
		}
	} else {
		if err != nil {
			t.Fatalf("%s: baseline: %v", f.id, err)
		}
		if !got.Valid || got.String != want {
			t.Fatalf("%s: got %#v, baseline %s", f.id, got, want)
		}
	}
}

func TestDraftSummaryProjectionAndEnsure(t *testing.T) {
	ctx := context.Background()
	env := pgtest.NewIsolated(t)
	for i := 0; i < 2; i++ {
		if err := draftsummary.EnsureDraftSummary(ctx, env.DB, env.Schema); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := env.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := draftsummary.EnsureDraftSummary(ctx, tx, env.Schema); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var kind, nullable string
	var def sql.NullString
	if err := env.DB.QueryRow(`SELECT data_type,is_nullable,column_default FROM information_schema.columns WHERE table_schema=$1 AND table_name='drafts' AND column_name='screen_summary'`, env.Schema).Scan(&kind, &nullable, &def); err != nil {
		t.Fatal(err)
	}
	if kind != "text" || nullable != "YES" || def.Valid {
		t.Fatalf("column: %s %s %#v", kind, nullable, def)
	}
	for _, f := range summaryFixtures() {
		t.Run(f.id, func(t *testing.T) {
			summaryExec(t, env.DB, `INSERT INTO drafts(id,json) VALUES($1,$2)`, f.id, f.raw)
			assertSummaryFixture(t, env, f)
		})
	}
	// Schema-qualified installation and calls also work with an unrelated path.
	conn, err := env.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	summaryExec(t, conn, `SET search_path TO pg_catalog`)
	if err := draftsummary.EnsureDraftSummary(ctx, conn, env.Schema); err != nil {
		t.Fatal(err)
	}
	summaryExec(t, conn, `INSERT INTO `+env.Schema+`.drafts(id,json) VALUES('qualified','{}')`)
	assertSummaryFixture(t, env, summaryFixture{"qualified", "{}", false})
	if err := draftsummary.EnsureDraftSummary(ctx, env.DB, `bad;schema`); err == nil {
		t.Fatal("invalid schema accepted")
	}
}

func TestDraftSummaryDirectWritesAndAtomicity(t *testing.T) {
	env := pgtest.NewIsolated(t)
	ctx := context.Background()
	raw := `{"title":"insert","body":"full"}`
	summaryExec(t, env.DB, `INSERT INTO drafts(id,json,screen_summary) VALUES('direct',$1,'caller supplied')`, raw)
	assertSummaryFixture(t, env, summaryFixture{"direct", raw, false})
	raw = `{"title":"update","proposal":null}`
	summaryExec(t, env.DB, `UPDATE drafts SET json=$1 WHERE id='direct'`, raw)
	assertSummaryFixture(t, env, summaryFixture{"direct", raw, false})
	raw = `{"title":"upsert","splitIssues":[]}`
	summaryExec(t, env.DB, `INSERT INTO drafts(id,json) VALUES('direct',$1) ON CONFLICT(id) DO UPDATE SET json=excluded.json`, raw)
	assertSummaryFixture(t, env, summaryFixture{"direct", raw, false})
	summaryExec(t, env.DB, `UPDATE drafts SET screen_summary='stale' WHERE id='direct'`)
	summaryExec(t, env.DB, `UPDATE drafts SET json=json WHERE id='direct'`)
	assertSummaryFixture(t, env, summaryFixture{"direct", raw, false})
	for _, commit := range []bool{false, true} {
		tx, err := env.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		changed := `{"title":"transaction"}`
		summaryExec(t, tx, `UPDATE drafts SET json=$1 WHERE id='direct'`, changed)
		summaryExec(t, tx, `INSERT INTO drafts(id,json) VALUES('transaction',$1)`, changed)
		var matches bool
		if err := tx.QueryRow(`SELECT bool_and(screen_summary=` + env.Schema + `.drafts_screen_summary(json)) FROM drafts WHERE id IN ('direct','transaction')`).Scan(&matches); err != nil || !matches {
			t.Fatalf("inside transaction: %v %v", matches, err)
		}
		assertSummaryFixture(t, env, summaryFixture{"direct", raw, false})
		var count int
		if err := env.DB.QueryRow(`SELECT count(*) FROM drafts WHERE id='transaction'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("uncommitted insert visible: %d %v", count, err)
		}
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			assertSummaryFixture(t, env, summaryFixture{"direct", changed, false})
			assertSummaryFixture(t, env, summaryFixture{"transaction", changed, false})
		} else {
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			assertSummaryFixture(t, env, summaryFixture{"direct", raw, false})
			if err := env.DB.QueryRow(`SELECT count(*) FROM drafts WHERE id='transaction'`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rolled back insert survived: %d %v", count, err)
			}
		}
	}
	// SQL errors from installation are preserved for callers, not suppressed.
	if err := draftsummary.EnsureDraftSummary(ctx, env.DB, "missing_summary_schema"); err == nil {
		t.Fatal("missing table error suppressed")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := draftsummary.EnsureDraftSummary(canceled, env.DB, env.Schema); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
