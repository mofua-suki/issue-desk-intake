package jobs

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

func transferFixture() (*transferJob, *transferJob, *transferJob) {
	var records map[string]any
	_ = json.Unmarshal([]byte(`{"source":"owner/repo#1","report":{"acceptanceTransfers":[{"criterion":"required artifact","artifacts":["output"],"followupNumbers":[1]}]},"proposals":[{"id":"p","number":1,"status":"filed","issue":{"project":"owner/repo","number":2}}]}`), &records)
	source := &transferJob{ID: 1, Repo: "owner/repo", Issue: 1, Status: "merged", View: map[string]any{"acceptanceTransferRecords": records}}
	destination := &transferJob{ID: 2, Repo: "owner/repo", Issue: 2, Status: "closed", View: map[string]any{}, Result: map[string]any{}}
	consumer := &transferJob{ID: 3, Repo: "owner/repo", Issue: 3, Title: "use artifact", Status: "queued", Deps: []int64{1}, View: map[string]any{"acceptanceTransferInput": map[string]any{"revision": "current", "complete": true}}}
	return source, destination, consumer
}
func TestTransferRequiresActualMergedArtifactAndCurrentEvidence(t *testing.T) {
	source, destination, consumer := transferFixture()
	all := []*transferJob{source, destination, consumer}
	plan := newTransferIndex(all).plan(consumer)
	if !plan.Blocking || len(plan.Checks) != 1 {
		t.Fatalf("unknown relation allowed %v", plan)
	}
	key := fleet.Text(fleet.Object(plan.Checks[0])["key"])
	consumer.View["acceptanceTransferChecks"] = map[string]any{key: map[string]any{"relation": "required", "evidence": "checked"}}
	plan = newTransferIndex(all).plan(consumer)
	if !plan.Blocking || len(plan.Targets) != 1 || plan.Targets[0] != 2 {
		t.Fatalf("unmerged close accepted %v", plan)
	}
	destination.Status = "merged"
	destination.Result["mergeCommit"] = strings.Repeat("a", 40)
	plan = newTransferIndex(all).plan(consumer)
	if plan.Blocking {
		t.Fatal("merged artifact blocked")
	}
	consumer.View["acceptanceTransferInput"] = map[string]any{"revision": "changed", "complete": true}
	if !newTransferIndex(all).plan(consumer).Blocking {
		t.Fatal("stale evidence accepted after request edit")
	}
}
func TestTransferProducerIsExemptButSiblingArtifactsStillBlock(t *testing.T) {
	source, destination, _ := transferFixture()
	destination.Deps = []int64{source.ID}
	if plan := newTransferIndex([]*transferJob{source, destination}).plan(destination); plan.Blocking || len(plan.Checks) != 0 {
		t.Fatal("producer blocked on its own artifact", plan)
	}
	records := fleet.Object(source.View["acceptanceTransferRecords"])
	transfer := fleet.Object(fleet.List(fleet.Object(records["report"])["acceptanceTransfers"])[0])
	transfer["followupNumbers"] = []any{1.0, 2.0}
	records["proposals"] = append(fleet.List(records["proposals"]), map[string]any{"id": "sibling", "number": 2.0, "status": "waiting"})
	if !newTransferIndex([]*transferJob{source, destination}).plan(destination).Blocking {
		t.Fatal("sibling dependency exempted with producer")
	}
}

func TestRunLimitHeldUsesWorkerRuns(t *testing.T) {
	reason := "question"
	job := &Job{Status: "held", HoldReason: &reason, Generation: 2, View: map[string]any{"workerExecution": map[string]any{"implementationRuns": float64(4)}}}
	if !RunLimitHeld(job, 4) || EffectiveRuns(job) != 4 {
		t.Fatal("worker runs must count", EffectiveRuns(job))
	}
	job.View = map[string]any{}
	if RunLimitHeld(job, 4) {
		t.Fatal("generation 2 is below the limit")
	}
	other := "review_held"
	job.HoldReason, job.Generation = &other, 9
	if RunLimitHeld(job, 4) {
		t.Fatal("only question holds are run limit holds")
	}
}

type transferWaitWriter struct{ saved []byte }

func (w *transferWaitWriter) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	w.saved = []byte(args[1].(string))
	return driver.RowsAffected(1), nil
}
func (w *transferWaitWriter) QueryRowContext(context.Context, string, ...any) *sql.Row { return nil }

func TestTransferMissingProposal(t *testing.T) {
	source, destination, consumer := transferFixture()
	proposal := fleet.Object(fleet.List(fleet.Object(source.View["acceptanceTransferRecords"])["proposals"])[0])
	proposal["status"] = "missing"
	index := newTransferIndex([]*transferJob{source, destination, consumer})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	key := fleet.Text(fleet.Object(index.planAt(consumer, now).Checks[0])["key"])
	consumer.View["acceptanceTransferChecks"] = map[string]any{key: map[string]any{"relation": "required"}}
	writer := &transferWaitWriter{}
	if err := applyTransferWait(context.Background(), writer, consumer, index, now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(writer.saved, &stored); err != nil {
		t.Fatal(err)
	}
	consumer.View["acceptanceTransferWait"] = stored
	first := index.planAt(consumer, now)
	if !first.Blocking || len(first.Waits) != 1 || len(first.OrphanedWaits) != 0 {
		t.Fatalf("first: %+v", first)
	}
	before := index.planAt(consumer, now.Add(59*time.Minute))
	if !before.Blocking || len(before.Waits) != 1 {
		t.Fatalf("grace: %+v", before)
	}
	if err := applyTransferWait(context.Background(), writer, consumer, index, now.Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(writer.saved, &stored); err != nil {
		t.Fatal(err)
	}
	consumer.View["acceptanceTransferWait"] = stored
	after := index.planAt(consumer, now.Add(time.Hour))
	if after.Blocking || len(after.Waits) != 0 || len(after.OrphanedWaits) != 1 || fleet.Object(after.OrphanedWaits[0])["missingSince"] != now.Format(time.RFC3339) {
		t.Fatalf("expired: %+v", after)
	}
	delete(consumer.View, "acceptanceTransferChecks")
	if p := index.planAt(consumer, now.Add(2*time.Hour)); !p.Blocking || len(p.OrphanedWaits) != 0 {
		t.Fatalf("unknown: %+v", p)
	}
	consumer.View["acceptanceTransferChecks"] = map[string]any{key: map[string]any{"relation": "required"}}
	proposal["status"] = "filed"
	if p := index.planAt(consumer, now.Add(2*time.Hour)); !p.Blocking || len(p.OrphanedWaits) != 0 {
		t.Fatalf("filed: %+v", p)
	}
}

func TestTransferMissingProposalParentGate(t *testing.T) {
	source, destination, consumer := transferFixture()
	proposal := fleet.Object(fleet.List(fleet.Object(source.View["acceptanceTransferRecords"])["proposals"])[0])
	proposal["status"] = "missing"
	destination.Status = "merged"
	destination.Result["mergeCommit"] = strings.Repeat("a", 40)
	consumer.Deps = []int64{source.ID, destination.ID}
	index := newTransferIndex([]*transferJob{source, destination, consumer})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	key := fleet.Text(fleet.Object(index.planAt(consumer, now).Checks[0])["key"])
	consumer.View["acceptanceTransferChecks"] = map[string]any{key: map[string]any{"relation": "required"}}
	first := index.planAt(consumer, now)
	consumer.View["acceptanceTransferWait"] = map[string]any{"missingSince": first.MissingSince}
	p := index.planAt(consumer, now.Add(time.Hour))
	if p.Blocking || len(p.Targets) != 0 || len(p.OrphanedWaits) != 1 {
		t.Fatalf("parent: %+v", p)
	}
}
