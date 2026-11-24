package consultationai

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"math"
	"time"
)

// Reconcile only durable measurements. A user reply may have released a hold,
// but must not discard the call ID or exclude its known usage from the budget.
func (s *Service) reconcileMeasurement(ctx context.Context, d Record, b *callBudget) error {
	if b == nil || s.options.ReadUsage == nil {
		return nil
	}
	if id := fleet.Text(fleet.Object(b.value["pendingCall"])["id"]); id != "" {
		if err := s.reconcileCallMeasurement(ctx, d, b, id, false); err != nil {
			return err
		}
	}
	for _, item := range fleet.Elements(b.value["measurementReleases"]) {
		id := fleet.Text(fleet.Object(item)["callId"])
		if id == "" {
			continue
		}
		if err := s.reconcileCallMeasurement(ctx, d, b, id, true); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) reconcileCallMeasurement(ctx context.Context, d Record, b *callBudget, id string, released bool) error {
	for _, item := range fleet.Elements(b.value["measurementReconciliations"]) {
		if fleet.Object(item)["callId"] == id {
			return nil
		}
	}
	row, err := s.options.ReadUsage(ctx, id)
	if err != nil {
		return err
	}
	if row == nil || row["callId"] != id || row["draft"] != d["id"] || row["source"] != "intake" || row["repo"] != d["project"] || row["usagePartial"] == true {
		return nil
	}
	amount := usage.Weighted(row, fleet.Object(b.config["usageWeights"]))["weighted"]
	if amount == nil {
		return nil
	}
	before := copyRecord(b.value)
	b.value["weighted"] = fleet.Number(b.value["weighted"]) + fleet.Number(amount)
	fields := Record{}
	if !released {
		b.value["unknownCalls"] = math.Max(0, fleet.Number(b.value["unknownCalls"])-1)
		delete(b.value, "pendingCall")
		fields["usageMeasurementHold"] = nil
	}
	history := append([]any{}, fleet.Elements(b.value["measurementReconciliations"])...)
	b.value["measurementReconciliations"] = append(history, Record{"callId": id, "at": s.options.Now().UTC().Format(time.RFC3339Nano), "weighted": amount})
	if err := b.persistWith(ctx, fields); err != nil {
		b.value = before
		return err
	}
	return nil
}
