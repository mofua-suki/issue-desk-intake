package operations

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestPostgresJobAttentionOrphanedWaits(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	ctx := context.Background()
	rows := []struct {
		issue        int
		status, view string
	}{
		{1, "queued", `{"acceptanceTransferWait":{"orphanedWaits":[{"proposalId":"split-123","reason":"missing proposal"}]}}`},
		{2, "queued", `{"acceptanceTransferWait":{"orphanedWaits":[]}}`},
		{3, "queued", `{"acceptanceTransferWait":{"orphanedWaits":{}}}`},
		{4, "queued", `{"acceptanceTransferWait":{"orphanedWaits":null}}`},
		{5, "queued", `{}`},
		{6, "merged", `{"acceptanceTransferWait":{"orphanedWaits":[{"proposalId":"other"}]}}`},
		{7, "queued", `{"acceptanceTransferWait":{"blocking":true,"orphanedWaits":"invalid","waits":[{"reason":"waiting"}]}}`},
	}
	for _, row := range rows {
		if _, err := s.PostgreSQL.ExecContext(ctx, `INSERT INTO jobs(repo,issue,title,status,view) VALUES('owner/repo',$1,'job',$2,$3::jsonb)`, row.issue, row.status, row.view); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.jobAttention(ctx, map[string]bool{"owner/repo": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("attention items: %v", items)
	}
	orphan := fleet.Object(items[0])
	if orphan["kind"] != "acceptance_transfer_orphan" || orphan["number"] != int64(1) || !strings.Contains(fleet.Text(orphan["reason"]), "split-123") {
		t.Fatalf("orphan attention: %v", orphan)
	}
	blocking := fleet.Object(items[1])
	if blocking["kind"] != "acceptance_transfer" || blocking["number"] != int64(7) {
		t.Fatalf("blocking attention: %v", blocking)
	}
}

func TestAcceptanceTransferOrphanAttention(t *testing.T) {
	orphan := map[string]any{"acceptanceTransferWait": map[string]any{"orphanedWaits": []any{map[string]any{"proposalId": "split-123", "reason": "受付に案の記録が無く起票されないため待たない"}}}}
	items := acceptanceTransferAttentionItems(1, "owner/repo", 1, "job", orphan, nil)
	if len(items) != 1 {
		t.Fatalf("items: %v", items)
	}
	item := fleet.Object(items[0])
	reason := fleet.Text(item["reason"])
	if item["kind"] != "acceptance_transfer_orphan" || !strings.Contains(reason, "split-123") || !strings.Contains(reason, "起票されない") {
		t.Fatalf("item: %v", item)
	}
	blocking := map[string]any{"acceptanceTransferWait": map[string]any{"blocking": true, "waits": []any{map[string]any{"proposalId": "p", "reason": "waiting"}}}}
	items = acceptanceTransferAttentionItems(2, "owner/repo", 2, "job", blocking, nil)
	if len(items) != 1 || fleet.Object(items[0])["kind"] != "acceptance_transfer" || !strings.Contains(fleet.Text(fleet.Object(items[0])["reason"]), "waiting") {
		t.Fatalf("blocking: %v", items)
	}
}
