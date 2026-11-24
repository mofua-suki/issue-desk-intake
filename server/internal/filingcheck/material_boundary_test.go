package filingcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/pgtest"
)

type ancestorBoundary struct {
	name                string
	count               int
	end                 string
	candidate, siblings bool
}

func ancestorBoundaries() []ancestorBoundary {
	return []ancestorBoundary{
		{name: "self", end: "target"},
		{name: "return-to-target", count: 3, end: "target"},
		{name: "ancestor-cycle", count: 3, end: "a002"},
		{name: "63", count: 63}, {name: "64", count: 64}, {name: "65", count: 65}, {name: "80", count: 80},
		{name: "missing", count: 2, end: "missing"},
		{name: "65-independent-candidate", count: 65, candidate: true},
		{name: "65-with-siblings", count: 65, siblings: true},
	}
}

// DB keys deliberately differ from JSON IDs. The candidate is outside the
// reference boundary and has an independent, eligible comparison role.
func seedAncestorBoundary(t testing.TB, records *consultations.Records, tc ancestorBoundary) (Record, map[string]string) {
	t.Helper()
	ctx := t.Context()
	if _, err := records.DB.ExecContext(ctx, "DELETE FROM drafts"); err != nil {
		t.Fatal(err)
	}
	target := Record{"id": "target", "project": "owner/repo", "createdAt": "2026-11-07T00:00:00Z", "status": "submitting", "body": "target body", "messages": []any{Record{"role": "user", "text": "target message"}}}
	if tc.count > 0 {
		target["parentConsultation"] = "a001"
	} else if tc.end != "" {
		target["parentConsultation"] = tc.end
	}
	rows := []Record{target}
	for i := 1; i <= tc.count; i++ {
		id := fmt.Sprintf("a%03d", i)
		r := Record{"id": id, "project": "elsewhere/repo", "status": "closed", "createdAt": "2026-11-01T00:00:00Z", "title": id, "body": "body " + id, "messages": []any{Record{"role": "user", "text": "message " + id}}}
		if i < tc.count {
			r["parentConsultation"] = fmt.Sprintf("a%03d", i+1)
		} else if tc.end != "" {
			r["parentConsultation"] = tc.end
		}
		if tc.candidate && i == 65 {
			r["project"] = "owner/repo"
			r["status"] = "draft"
		}
		rows = append(rows, r)
	}
	if tc.siblings {
		target["parentSplit"] = "a001"
		rows[1]["splitChildren"] = []any{"filed", "target", "unfiled", "missing-sibling"}
		rows[1]["splitIssues"] = []any{Record{}, Record{}, Record{}, Record{}}
		rows = append(rows, Record{"id": "filed", "parentSplit": "a001", "project": "owner/repo", "issue": Record{"project": "owner/repo", "number": 8}}, Record{"id": "unfiled", "parentSplit": "a001", "project": "owner/repo", "title": "unfiled", "body": "sibling body", "messages": []any{"sibling message"}})
	}
	full := map[string]string{}
	for _, r := range rows {
		key := "db-" + fleet.Text(r["id"])
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = records.DB.ExecContext(ctx, "INSERT INTO drafts(id,json) VALUES($1,$2)", key, string(raw)); err != nil {
			t.Fatal(err)
		}
		full[key] = string(raw)
	}
	return target, full
}
func TestMaterialAncestorBoundaryReads(t *testing.T) {
	env := pgtest.NewIsolated(t)
	records, err := consultations.OpenRecords(t.Context(), env.DB)
	if err != nil {
		t.Fatal(err)
	}
	observed, counts := observedMaterialRecords(t, env)
	for _, tc := range ancestorBoundaries() {
		t.Run(tc.name, func(t *testing.T) {
			target, full := seedAncestorBoundary(t, records, tc)
			*counts = materialObservation{}
			got, err := readMaterialDrafts(t.Context(), observed, target)
			if err != nil {
				t.Fatal(err)
			}
			expected := map[string]bool{}
			for i := 1; i <= tc.count && i <= 64; i++ {
				expected[fmt.Sprintf("db-a%03d", i)] = true
			}
			if tc.candidate {
				expected["db-a065"] = true
			}
			if tc.siblings {
				expected["db-unfiled"] = true
			}
			wantBytes := 0
			for key := range expected {
				wantBytes += len(key) + len(full[key])
			}
			if !reflect.DeepEqual(counts.Keys, expected) && !(len(counts.Keys) == 0 && len(expected) == 0) || counts.Selected.Rows != len(expected) || counts.Selected.Bytes != wantBytes || counts.Projection.Rows != len(full) || counts.Fallback.Calls != 0 || counts.Commits != 1 || counts.Rollbacks != 0 || counts.Hold <= 0 {
				t.Fatalf("actual full DB read=%+v keys=%v want=%v bytes=%d", counts, counts.Keys, expected, wantBytes)
			}
			// Also reject bodies/messages leaking through the normal projection.
			for _, row := range got {
				if !expected["db-"+fleet.Text(row["id"])] {
					if _, ok := row["body"]; ok {
						t.Fatal("unselected body in projection", row["id"])
					}
					if _, ok := row["messages"]; ok {
						t.Fatal("unselected messages in projection", row["id"])
					}
				}
			}
			legacy, err := records.List(t.Context(), "drafts")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decisionContext(target, got), decisionContext(target, legacy)) {
				t.Fatal("ancestor context differs from full read")
			}
			refs := fleet.Elements(decisionContext(target, got)["references"])
			want := tc.count
			if tc.end == "missing" {
				want++
			}
			if want > 64 {
				want = 64
			}
			if len(refs) != want {
				t.Fatalf("references=%d want=%d", len(refs), want)
			}
			if tc.end == "missing" && obj(refs[len(refs)-1])["unavailable"] != true {
				t.Fatal("missing parent lost")
			}
		})
	}
}
func TestMaterialSnapshotFallbackAndReadErrors(t *testing.T) {
	env := pgtest.NewIsolated(t)
	records, err := consultations.OpenRecords(t.Context(), env.DB)
	if err != nil {
		t.Fatal(err)
	}
	observed, counts := observedMaterialRecords(t, env)
	target, _ := seedAncestorBoundary(t, records, ancestorBoundary{count: 65})
	for _, tc := range []struct {
		name, raw, state string
		fails            bool
	}{
		{"unicode", `{"id":"exception","title":"\ud800"}`, "22P02", false},
		{"nul-key", `{"id":"exception","\u0000":"value"}`, "22P05", false},
		{"invalid-json", `{`, "22P02", true},
		{"non-object", `[]`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() {
				if _, err := records.DB.ExecContext(context.Background(), "DELETE FROM drafts WHERE id='exception'"); err != nil {
					t.Error(err)
				}
			})
			if _, err := records.DB.ExecContext(t.Context(), "INSERT INTO drafts(id,json) VALUES('exception',$1)", tc.raw); err != nil {
				t.Fatal(err)
			}
			*counts = materialObservation{}
			got, err := readMaterialDrafts(t.Context(), observed, target)
			_, legacyErr := records.List(t.Context(), "drafts")
			if (err != nil) != tc.fails || (legacyErr != nil) != tc.fails || counts.ProjectionState != tc.state || (counts.Fallback.Calls == 1) != (tc.state != "") {
				t.Fatalf("err=%v legacy=%v counts=%+v", err, legacyErr, counts)
			}
			if tc.fails {
				if counts.Commits != 0 || counts.Rollbacks != 1 {
					t.Fatal(counts)
				}
			} else {
				if counts.Fallback.Rows != 67 || counts.Fallback.Bytes == 0 || counts.Selected.Rows != 64 || counts.Commits != 1 {
					t.Fatal(counts)
				}
				legacy, _ := records.List(t.Context(), "drafts")
				if !reflect.DeepEqual(decisionContext(target, got), decisionContext(target, legacy)) {
					t.Fatal("fallback context changed")
				}
			}
			if _, err := records.DB.ExecContext(context.Background(), "DELETE FROM drafts WHERE id='exception'"); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, kind := range []string{"projection", "selected"} {
		t.Run(kind, func(t *testing.T) {
			*counts = materialObservation{Fail: kind}
			_, err := readMaterialDrafts(t.Context(), observed, target)
			if err == nil || err.Error() != "injected material read failure" || counts.Fallback.Calls != 0 || counts.Commits != 0 || counts.Rollbacks != 1 || counts.Hold <= 0 {
				t.Fatalf("%v %+v", err, counts)
			}
		})
	}
}

func TestMaterialSnapshotDatabaseKeys(t *testing.T) {
	env := pgtest.NewIsolated(t)
	records, err := consultations.OpenRecords(t.Context(), env.DB)
	if err != nil {
		t.Fatal(err)
	}
	observed, counts := observedMaterialRecords(t, env)
	for i, id := range []string{`"duplicate"`, `"duplicate"`, `42`, `null`} {
		raw := fmt.Sprintf(`{"id":%s,"project":"owner/repo","status":"draft","createdAt":"1","body":"body-%d","messages":["message-%d"]}`, id, i, i)
		if _, err := env.DB.ExecContext(t.Context(), "INSERT INTO drafts(id,json) VALUES($1,$2)", fmt.Sprintf("key-%d", i), raw); err != nil {
			t.Fatal(err)
		}
	}
	target := Record{"id": "target", "project": "owner/repo", "createdAt": "2"}
	got, err := readMaterialDrafts(t.Context(), observed, target)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := records.List(t.Context(), "drafts")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, legacy) || counts.Selected.Rows != 4 || len(counts.Keys) != 4 || counts.Fallback.Calls != 0 {
		t.Fatalf("DB key mapping lost: got=%v legacy=%v counts=%+v", got, legacy, counts)
	}
}
