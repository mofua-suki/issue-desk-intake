package filingcheck

import (
	"context"
	"database/sql"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"testing"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/pgtest"
)

type failingMaterialReader struct{}

func (failingMaterialReader) ReadFilingSnapshot(context.Context, func([]consultations.DraftFields) []string) ([]consultations.Record, error) {
	return nil, errors.New("read failed")
}

func TestMaterialDraftReadPropagatesErrors(t *testing.T) {
	if _, err := readMaterialDrafts(t.Context(), failingMaterialReader{}, nil); err == nil {
		t.Fatal("read error lost")
	}
}

func TestMaterialCandidateBoundaries(t *testing.T) {
	d := Record{"id": "target", "project": "Owner/Repo", "createdAt": "2"}
	base := Record{"id": "prior", "project": "owner/repo", "createdAt": "1", "status": "draft"}
	cases := []struct {
		name   string
		mutate func(Record)
		family bool
		want   bool
	}{
		{"eligible", func(Record) {}, false, true},
		{"family", func(Record) {}, true, false},
		{"same id", func(r Record) { r["id"] = "target" }, false, false},
		{"other project", func(r Record) { r["project"] = "other" }, false, false},
		{"issue", func(r Record) { r["issue"] = Record{} }, false, false},
		{"issues", func(r Record) { r["issues"] = []any{1} }, false, false},
		{"split", func(r Record) { r["parentSplit"] = "p" }, false, false},
		{"inactive", func(r Record) { r["status"] = "closed" }, false, false},
		{"later", func(r Record) { r["createdAt"] = "3" }, false, false},
		{"tie before", func(r Record) { r["createdAt"] = "2" }, false, true},
		{"tie after", func(r Record) { r["createdAt"] = "2"; r["id"] = "z" }, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := clone(base)
			tc.mutate(r)
			family := map[string]bool{}
			if tc.family {
				family[fleet.Text(r["id"])] = true
			}
			if got := materialCandidate(d, r, family, materialComparisonAt(d, nil)); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

// updatingMaterialReader changes both an eligible and an ineligible row after
// the first read, reproducing an update between projection and full reads.
type updatingMaterialReader struct {
	*consultations.Records
}

func (r *updatingMaterialReader) update(ctx context.Context) error {
	for _, row := range []consultations.Record{
		{"id": "prior", "project": "other", "createdAt": "0", "status": "closed", "body": "new body"},
		{"id": "other", "project": "p", "createdAt": "0", "status": "draft", "body": "new candidate"},
	} {
		_, err := r.Records.Change(ctx, "drafts", row["id"].(string), func(*sql.Tx, consultations.Record) (consultations.Record, error) { return row, nil })
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *updatingMaterialReader) ReadFilingSnapshot(ctx context.Context, selectKeys func([]consultations.DraftFields) []string) ([]consultations.Record, error) {
	var updateErr error
	rows, err := r.Records.ReadFilingSnapshot(ctx, func(entries []consultations.DraftFields) []string {
		keys := selectKeys(entries)
		updateErr = r.update(ctx)
		return keys
	})
	if updateErr != nil {
		return nil, updateErr
	}
	return rows, err
}

func TestMaterialDraftsConcurrentUpdateKeepsOneSnapshot(t *testing.T) {
	ctx := t.Context()
	records, err := consultations.OpenRecords(ctx, pgtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []consultations.Record{
		{"id": "prior", "project": "p", "createdAt": "0", "status": "draft", "body": "old body"},
		{"id": "other", "project": "other", "createdAt": "0", "status": "closed", "body": "old other"},
	} {
		_, err = records.Change(ctx, "drafts", row["id"].(string), func(*sql.Tx, consultations.Record) (consultations.Record, error) { return row, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	target := Record{"id": "target", "project": "p", "createdAt": "2"}
	got, err := readMaterialDrafts(ctx, &updatingMaterialReader{records}, target)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Record{}
	for _, row := range got {
		byID[fleet.Text(row["id"])] = row
	}
	if byID["prior"]["body"] != "old body" || !materialCandidate(target, byID["prior"], nil, "2") {
		t.Fatalf("candidate mixed snapshots: %#v", byID["prior"])
	}
	if _, ok := byID["other"]["body"]; ok {
		t.Fatalf("unrelated body loaded: %#v", byID["other"])
	}
	if materialCandidate(target, byID["other"], nil, "2") {
		t.Fatalf("new candidate leaked into snapshot: %#v", byID["other"])
	}
	after, err := readMaterialDrafts(ctx, records, target)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range after {
		if fleet.Text(row["id"]) == "other" && (row["body"] != "new candidate" || !materialCandidate(target, row, nil, "2")) {
			t.Fatalf("new candidate missing: %#v", row)
		}
	}
}

func TestMaterialDraftSelectionPreservesFamilyAndAvoidsUnrelatedBodies(t *testing.T) {
	ctx := t.Context()
	records, err := consultations.OpenRecords(ctx, pgtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	rows := []Record{
		{"id": "ancestor", "body": "ancestor body", "messages": []any{"ancestor message"}},
		{"id": "parent", "createdAt": "1", "parentConsultation": "ancestor", "splitChildren": []any{"target", "sibling"}, "body": "parent body"},
		{"id": "sibling", "parentSplit": "parent", "project": "p", "createdAt": "1", "status": "draft", "ready": true, "body": "sibling body", "messages": []any{"sibling message"}},
		{"id": "prior", "project": "p", "createdAt": "0", "status": "draft", "body": "candidate body", "messages": []any{"candidate message"}},
		{"id": "unrelated", "project": "other", "createdAt": "0", "status": "draft", "body": "unrelated body", "messages": []any{"unrelated message"}},
	}
	for _, row := range rows {
		_, err = records.Change(ctx, "drafts", fleet.Text(row["id"]), func(*sql.Tx, consultations.Record) (consultations.Record, error) { return row, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	target := Record{"id": "target", "parentSplit": "parent", "project": "p", "createdAt": "2"}
	got, err := readMaterialDrafts(ctx, records, target)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Record{}
	for _, row := range got {
		byID[fleet.Text(row["id"])] = row
	}
	for _, id := range []string{"ancestor", "parent", "prior"} {
		if byID[id]["body"] == nil {
			t.Errorf("%s body missing: %#v", id, byID[id])
		}
	}
	if byID["prior"]["messages"] == nil || byID["ancestor"]["messages"] == nil {
		t.Fatal("selected messages missing")
	}
	if byID["sibling"]["body"] != "sibling body" {
		t.Fatal("sibling body missing")
	}
	for _, id := range []string{"unrelated"} {
		if _, ok := byID[id]["body"]; ok {
			t.Errorf("%s body loaded", id)
		}
		if _, ok := byID[id]["messages"]; ok {
			t.Errorf("%s messages loaded", id)
		}
	}
	if len(familyMembers(target, got)) != 3 || byID["sibling"]["ready"] != true {
		t.Fatalf("family projection lost: %#v", got)
	}
}
