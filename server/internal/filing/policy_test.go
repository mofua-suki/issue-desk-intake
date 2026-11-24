package filing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"strings"
	"testing"
)

func TestFilingPolicyUsesAuthoritativeIncidentLedgerAndDoesNotFallback(t *testing.T) {
	ctx := context.Background()
	records := recordsFixture(t)
	d := Record{"id": "manual"}
	if _, err := records.DB.Exec(`INSERT INTO settings(id,json) VALUES('incident-deadline','{"value":[{"actions":[{"action":"file","id":"manual"}]}]}')`); err != nil {
		t.Fatal(err)
	}
	if err := CheckPolicy(ctx, records, d, func(context.Context) (any, error) { return []any{}, nil }); err != nil {
		t.Fatal("obsolete local setting overrode shared ledger", err)
	}
	if err := CheckPolicy(ctx, records, d, func(context.Context) (any, error) {
		return []any{Record{"actions": []any{Record{"action": "file", "id": "manual"}}}}, nil
	}); err == nil {
		t.Fatal("shared provenance ignored")
	}
	want := errors.New("shared unavailable")
	if err := CheckPolicy(ctx, records, d, func(context.Context) (any, error) { return nil, want }); !errors.Is(err, want) {
		t.Fatal("failed shared read fell back to local settings", err)
	}
}

func recordsFixture(t *testing.T) *consultations.Records {
	t.Helper()
	records, e := consultations.OpenRecords(context.Background(), pgtest.New(t))
	if e != nil {
		t.Fatal(e)
	}
	return records
}
func persist(t *testing.T, records *consultations.Records, d Record) {
	t.Helper()
	_, e := records.Change(context.Background(), "drafts", fleet.Text(d["id"]), func(*sql.Tx, Record) (Record, error) { return d, nil })
	if e != nil {
		t.Fatal(e)
	}
}
func TestFilingPolicyAncestorsLedgersAndBasis(t *testing.T) {
	ctx := context.Background()
	records := recordsFixture(t)
	root := Record{"id": "root"}
	persist(t, records, root)
	child := Record{"id": "child", "parentSplit": "root"}
	if e := CheckPolicy(ctx, records, child); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"proposal", "attention", "usageReviewRound", "priorityReviewRound", "inventoryRound", "stallFix", "fullTestFix", "fullTestRun", "appHealth", "duplicateFalsePositive"} {
		root[key] = true
		persist(t, records, root)
		if e := CheckPolicy(ctx, records, child); e == nil {
			t.Fatal("generated ancestor accepted", key)
		}
		delete(root, key)
	}
	persist(t, records, root)
	root["parentConsultation"] = "child"
	persist(t, records, root)
	persist(t, records, child)
	if e := CheckPolicy(ctx, records, child); e == nil {
		t.Fatal("cycle accepted")
	}
	delete(root, "parentConsultation")
	persist(t, records, root)
	child["parentSplit"] = "missing"
	if e := CheckPolicy(ctx, records, child); e == nil {
		t.Fatal("missing ancestor accepted")
	}
	child["parentSplit"] = "root"
	if _, e := records.DB.Exec(`INSERT INTO temporary_code_records(repo,item,stage,json) VALUES('owner/app','record','done','{"request":{"id":"root"}}')`); e != nil {
		t.Fatal(e)
	}
	if e := CheckPolicy(ctx, records, child); e == nil {
		t.Fatal("temporary-code provenance accepted")
	}
	if _, e := records.DB.Exec(`DELETE FROM temporary_code_records`); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(Record{"value": []any{Record{"actions": []any{Record{"action": "file", "id": "root"}}}}})
	if _, e := records.DB.Exec(`INSERT INTO settings(id,json) VALUES('incident-deadline',?)`, string(raw)); e != nil {
		t.Fatal(e)
	}
	if e := CheckPolicy(ctx, records, child); e == nil {
		t.Fatal("incident provenance accepted")
	}
	if _, e := records.DB.Exec(`DELETE FROM settings WHERE id='incident-deadline'`); e != nil {
		t.Fatal(e)
	}
	child["basisConflicts"] = []any{Record{"fact": "対象", "reason": "違う", "kind": "user_decision", "resolution": nil}}
	if e := CheckPolicy(ctx, records, child); e == nil {
		t.Fatal("unresolved conflict filed")
	}
	// A reflected correction settles the conflict without waiting for the user.
	child["basisConflicts"] = []any{Record{"fact": "対象", "reason": "違う", "kind": "user_decision", "resolution": "訂正を反映済み"}}
	if e := CheckPolicy(ctx, records, child); e != nil {
		t.Fatal(e)
	}
	child["basisConflicts"] = []any{Record{"fact": "対象", "reason": "訂正", "kind": "requester_fact", "resolution": "保存済み"}}
	if e := CheckPolicy(ctx, records, child); e != nil {
		t.Fatal(e)
	}
}
func TestFilingPolicyDepthAndRetiredInvestigation(t *testing.T) {
	ctx := context.Background()
	records := recordsFixture(t)
	for i := 0; i < 66; i++ {
		d := Record{"id": fmt.Sprint(i)}
		if i < 65 {
			d["parentSplit"] = fmt.Sprint(i + 1)
		}
		persist(t, records, d)
	}
	if e := CheckPolicy(ctx, records, Record{"id": "start", "parentSplit": "0"}); e == nil {
		t.Fatal("overlong ancestor chain accepted")
	}
	for _, d := range []Record{{"kind": "investigation"}, {"workType": "investigation"}, {"view": Record{"workType": "investigation"}}, {"status": "investigation_reported"}, {"body": "<!-- issue-desk:investigation -->\n<!-- issue-desk:11111111-1111-4111-8111-111111111111 -->"}} {
		if e := CheckPolicy(ctx, records, d); e == nil || !strings.Contains(e.Error(), "廃止") {
			t.Fatal("retired work accepted", d, e)
		}
	}
}
func TestInvestigationMarkerUsesOnlyFinalTrailer(t *testing.T) {
	quoted := "<!-- issue-desk:investigation -->\n<!-- issue-desk:11111111-1111-4111-8111-111111111111 -->"
	if !investigationBody(quoted) {
		t.Fatal("旧調査の末尾を認識できません")
	}
	body := quoted + "\n\n通常の本文\n<!-- issue-desk:22222222-2222-4222-8222-222222222222 -->"
	if investigationBody(body) {
		t.Fatal("引用した昔の組を新しい調査として扱いました")
	}
	if e := assertManual(Record{"body": body, "usageReviewRound": float64(0)}); e != nil {
		t.Fatal(e)
	}
}
