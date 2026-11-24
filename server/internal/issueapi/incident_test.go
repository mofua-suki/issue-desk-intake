package issueapi

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"testing"
)

func TestIncidentActionPreservesExplicitStopAndReceiptPostgres(t *testing.T) {
	f := newFixture(t)
	if _, e := f.pg.Exec(`UPDATE jobs SET status='running',lease_host='nas',lease_epoch=4 WHERE id=$1`, f.job); e != nil {
		t.Fatal(e)
	}
	if _, e := f.pg.Exec(`INSERT INTO job_commands(id,repo,issue,kind,payload,target_host) VALUES('00000000-0000-4000-8000-000000000091','owner/repo',1,'stop','{}','nas')`); e != nil {
		t.Fatal(e)
	}
	current, e := f.s.ReadIncidentTarget(f.ctx, "owner/repo", 1)
	if e != nil || current["stopped"] != true {
		t.Fatalf("stop observation %v %v", current, e)
	}
	if _, e = f.s.SendIncidentAction(f.ctx, "owner/repo", 1, "00000000-0000-4000-8000-000000000092", "reply", "確認してください"); e == nil {
		t.Fatal("pending stop bypassed")
	}
	// The row-lock control check protects the gap between observation and mutation.
	_, e = f.options.Jobs.Control(f.ctx, f.job, jobs.ControlInput{ID: "00000000-0000-4000-8000-000000000093", Action: "instruction", Text: "確認", By: "有事の期限切れの対処", Automatic: true}, jobs.ControlOptions{})
	if e == nil {
		t.Fatal("control allowed an automatic operation with a stop command")
	}
	if _, e = f.pg.Exec(`UPDATE jobs SET status='queued',lease_host=NULL WHERE id=$1`, f.job); e != nil {
		t.Fatal(e)
	}
	if _, e = f.pg.Exec(`UPDATE job_commands SET status='done'`); e != nil {
		t.Fatal(e)
	}
	first, e := f.s.SendIncidentAction(f.ctx, "owner/repo", 1, "00000000-0000-4000-8000-000000000094", "reply", "確認してください")
	if e != nil {
		t.Fatal(e)
	}
	f.restart()
	same, e := f.s.SendIncidentAction(f.ctx, "owner/repo", 1, "00000000-0000-4000-8000-000000000094", "reply", "確認してください")
	if e != nil || same["id"] != first["id"] || f.api.commentCalls != 1 {
		t.Fatalf("receipt %v %v", same, e)
	}
	if _, e = f.s.SendIncidentAction(f.ctx, "owner/repo", 1, "00000000-0000-4000-8000-000000000095", "approve-permissions", "許可"); e == nil {
		t.Fatal("permission action accepted")
	}
	if _, e = f.s.SendIncidentAction(f.ctx, "owner/repo", 1, "00000000-0000-4000-8000-000000000096", "reply", "/codex resume"); e == nil {
		t.Fatal("embedded resume command accepted")
	}
}

func TestRecoveryActionRejectsUnsafeOperationsPostgres(t *testing.T) {
	f := newFixture(t)
	for _, action := range []string{"notify", "approve-permissions", "stop"} {
		_, err := f.s.SendRecoveryAction(f.ctx, "owner/repo", 1, "00000000-0000-4000-8000-000000000097", action, "text")
		var p *contract.Problem
		if !errors.As(err, &p) || p.Status != 403 {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if _, err := f.pg.Exec(`UPDATE jobs SET status='stopped' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.SendRecoveryAction(f.ctx, "owner/repo", 1, "00000000-0000-4000-8000-000000000098", "retry", "text")
	var p *contract.Problem
	if !errors.As(err, &p) || p.Status != 409 {
		t.Fatal(err)
	}
}

func TestRecoveryActionRespectsRetryLimitPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pg.Exec(`UPDATE jobs SET status='failed' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.SendRecoveryAction(f.ctx, "owner/repo", 1, "00000000-0000-4000-8000-000000000099", "retry", "再試行")
	var p *contract.Problem
	if !errors.As(err, &p) || p.Status != 409 {
		t.Fatalf("retry limit must reject recovery: %v", err)
	}
}

func TestRecoveryActionReplyUsesRecoveryActorPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pg.Exec(`UPDATE jobs SET status='held',hold_reason='question',result='{"status":"question","questions":["何を確認しますか"]}' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-000000000100"
	_, err := f.s.SendRecoveryAction(f.ctx, "owner/repo", 1, id, "reply", "回答します")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := f.s.read(f.ctx, "actions", id)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.ObjectOrEmpty(receipt["payload"])["via"] != "止まった依頼の自動復旧" || f.api.commentCalls != 1 {
		t.Fatal(receipt, f.api.commentCalls)
	}
	if _, err := f.pg.Exec(`UPDATE jobs SET status='held',hold_reason='question',result='{}' WHERE id=$1`, f.job); err != nil {
		t.Fatal(err)
	}
	f.restart()
	_, err = f.s.SendRecoveryAction(f.ctx, "owner/repo", 1, id, "reply", "回答します")
	if err != nil || f.api.commentCalls != 1 {
		t.Fatal(err, f.api.commentCalls)
	}
}

func TestRecoverySendAdapterPreservesProblemsPostgres(t *testing.T) {
	f := newFixture(t)
	send := func(ctx context.Context, repo string, n int64, id, action, text string) error {
		_, err := f.s.SendRecoveryAction(ctx, repo, n, id, action, text)
		if err != nil {
			return fmt.Errorf("recovery send: %w", err)
		}
		return nil
	}
	for _, tc := range []struct {
		action string
		status int
	}{{"notify", 403}, {"retry", 409}} {
		if tc.status == 409 {
			if _, err := f.pg.Exec(`UPDATE jobs SET status='stopped' WHERE id=$1`, f.job); err != nil {
				t.Fatal(err)
			}
		}
		err := send(f.ctx, "owner/repo", 1, "00000000-0000-4000-8000-000000000101", tc.action, "text")
		var p *contract.Problem
		if !errors.As(err, &p) || p.Status != tc.status {
			t.Fatalf("%s: %v", tc.action, err)
		}
	}
}
