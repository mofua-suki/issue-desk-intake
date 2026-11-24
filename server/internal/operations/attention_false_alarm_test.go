package operations

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
)

func recoveryNotify(t *testing.T, r *JobRecovery, state string) {
	t.Helper()
	ctx := context.Background()
	candidates, err := r.Candidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range candidates {
		number := c.Job.Issue
		a := &RecoveryActions{Recovery: r, ReadTarget: func(context.Context, string, int64) (map[string]any, error) {
			return map[string]any{"project": "owner/repo", "number": float64(number), "status": "failed", "state": state}, nil
		}, Judge: func(_ context.Context, items []any) (map[string]any, error) {
			if state == "closed" {
				t.Fatal("閉じた依頼をAIに判断させた")
			}
			return map[string]any{"actions": []any{map[string]any{"item": fleet.Object(items[0])["key"], "action": "notify", "reason": "理由", "text": "確認してください。", "evidence": "記録", "requiresUser": true, "resolved": false}}}, nil
		}}
		if _, err := a.Act(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecoveryAttentionHidesClosedIssuesPostgres(t *testing.T) {
	r := recoveryFixture(t)
	r.Service.Gitea = urgentIssueReader{}
	for issue := 1; issue <= 3; issue++ {
		recoveryInsert(t, r, issue, "failed", nil, "normal", "owner/repo", `{}`, `{"error":"broken"}`, 0, nil, "1 hour")
	}
	recoveryNotify(t, r, "open")
	rows, err := r.Service.jobRecoveryAttention(context.Background(), map[string]bool{"owner/repo": true})
	if err != nil {
		t.Fatal(err)
	}
	// Issue 1 is closed; issue 3's lookup fails and must stay visible.
	if len(rows) != 2 {
		t.Fatal(rows)
	}
	for _, raw := range rows {
		row := fleet.Object(raw)
		if row["number"] == int64(1) || row["number"] == float64(1) {
			t.Fatal("閉じた依頼を表示した", row)
		}
		if label := fleet.Text(row["stateLabel"]); strings.Contains(label, "利用者") || label == "" {
			t.Fatal(label)
		}
	}
}

func TestRecoveryActSkipsClosedIssueBeforeJudgingPostgres(t *testing.T) {
	r := recoveryFixture(t)
	recoveryInsert(t, r, 1, "failed", nil, "normal", "owner/repo", `{}`, `{"error":"broken"}`, 0, nil, "1 hour")
	recoveryNotify(t, r, "closed")
	rows, err := r.Service.jobRecoveryAttention(context.Background(), map[string]bool{"owner/repo": true})
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
}

func TestRecoveryStateLabelNeverClaimsUserDecision(t *testing.T) {
	for _, state := range []string{"skipped", "abandoned", "send_unknown", "send_rejected"} {
		if label := recoveryStateLabel(state); label == "" || strings.Contains(label, "利用者") {
			t.Fatal(state, label)
		}
	}
}

func TestPostgresCanaryAlertStopsAfterEveryPCMovedPast(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	failed, previous, newer := strings.Repeat("e", 40), strings.Repeat("a", 40), strings.Repeat("c", 40)
	tag := func(revision string) string { return revision[:12] + "-client-python" }
	started := s.now().Add(-48 * time.Hour)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.PostgreSQL.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO release_rollouts(host,flavor,revision,canary,status,previous,started_at,reason) VALUES('*','-python',$1,'pc1','failed',$2::jsonb,$3,'timeout')`, failed, fmt.Sprintf(`{"tag":%q,"revision":%q}`, tag(previous), previous), started)
	host := func(name, current string) {
		t.Helper()
		exec(`INSERT INTO hosts(host,revision,heartbeat_at,ready,capacity,hostd_health) VALUES($1,'x',$2,true,1,$3::jsonb)`, name, s.now(), fmt.Sprintf(`{"cycle":{"versions":{"current":%q}}}`, current))
	}
	release := func(name, revision string, at time.Time) {
		t.Helper()
		exec(`INSERT INTO releases(host,flavor,registry,tag,digest,revision,kind,created_at) VALUES($1,'-python','registry',$2,$3,$4,'align',$5)`, name, tag(revision), "sha256:"+strings.Repeat("d", 64), revision, at)
	}
	count := func() int {
		t.Helper()
		rows, err := s.canaryAlerts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if count() != 1 {
		t.Fatal("PCの報告が無いのに失敗の行を消した")
	}
	host("pc1", tag(newer))
	host("pc2", tag(newer))
	release("pc1", newer, started.Add(time.Hour))
	if count() != 1 {
		t.Fatal("個別の新しい版が無いPCがあるのに失敗の行を消した")
	}
	release("pc2", newer, started.Add(time.Hour))
	if count() != 0 {
		t.Fatal("全PCが新しい版へ進んでも失敗の行が残った")
	}
	exec(`UPDATE hosts SET hostd_health=$1::jsonb WHERE host='pc2'`, fmt.Sprintf(`{"cycle":{"versions":{"current":%q}}}`, tag(previous)))
	if count() != 1 {
		t.Fatal("旧版のままのPCがあるのに失敗の行を消した")
	}
	exec(`UPDATE hosts SET hostd_health=$1::jsonb WHERE host='pc2'`, fmt.Sprintf(`{"cycle":{"versions":{"current":%q}}}`, tag(newer)))
	exec(`UPDATE releases SET created_at=$1 WHERE host='pc2'`, started.Add(-time.Hour))
	if count() != 1 {
		t.Fatal("失敗より前の個別の版を新しい版として扱った")
	}
}

func TestPostgresUrgentHoldReasonFallsBackToSummary(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	s.Gitea = urgentIssueReader{}
	updated := s.now().Add(-time.Hour)
	if _, err := s.PostgreSQL.Exec(`INSERT INTO jobs(repo,issue,title,status,priority,result,updated_at) VALUES('owner/repo',2,'summary','failed',1,'{"summary":"固定テストが不合格です"}'::jsonb,$1),('owner/repo',4,'error','failed',1,'{"error":"エラーの理由","summary":"要約"}'::jsonb,$1),('owner/repo',5,'none','failed',1,'{"error":"  "}'::jsonb,$1)`, updated); err != nil {
		t.Fatal(err)
	}
	rows, err := s.urgentHolds(t.Context())
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	want := map[string]string{"summary": "固定テストが不合格です", "error": "エラーの理由", "none": "理由の記録なし"}
	for _, raw := range rows {
		detail := fleet.Text(fleet.Object(raw)["detail"])
		found := false
		for title, reason := range want {
			if strings.Contains(detail, " "+title+"\n") {
				found = true
				if !strings.Contains(detail, "保留・失敗の理由: "+reason+"\n") {
					t.Fatal(detail)
				}
			}
		}
		if !found {
			t.Fatal(detail)
		}
	}
}

func TestPostgresDispatchAlertsExcludeTransferWait(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	s.StartedAt = s.now().Add(-3 * time.Hour)
	s.Assignments = &fleet.Assignments{Hosts: map[string]*fleet.Host{"worker": {Max: 1}}, Implementers: map[string][]string{"owner/repo": {"worker"}}, Output: map[string]any{}}
	updated := s.now().Add(-2 * time.Hour)
	if _, err := s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,status,view,updated_at) VALUES(1,'owner/repo',1,'queued','{"acceptanceTransferWait":{"blocking":true}}'::jsonb,$1)`, updated); err != nil {
		t.Fatal(err)
	}
	// Another job's lease makes the per-job stall check reach the waiting job.
	if _, err := s.PostgreSQL.Exec(`INSERT INTO jobs(id,repo,issue,status) VALUES(2,'owner/repo',2,'merged')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostgreSQL.Exec(`INSERT INTO job_events(job_id,kind,host,at,data) VALUES(2,'leased','worker',$1,'{}')`, s.now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	read := func() (idle, stalled int, detail string) {
		t.Helper()
		alerts, err := s.dispatchAlerts(context.Background(), map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range alerts {
			row := fleet.Object(raw)
			switch row["kind"] {
			case "dispatch_idle":
				idle++
				detail = fleet.Text(row["detail"])
			case "job_dispatch_stall":
				stalled++
			}
		}
		return idle, stalled, detail
	}
	if idle, stalled, _ := read(); idle != 0 || stalled != 0 {
		t.Fatalf("移管待ちを配布止まりとして表示した idle=%d stalled=%d", idle, stalled)
	}
	if _, err := s.PostgreSQL.Exec(`UPDATE jobs SET view='{"acceptanceTransferWait":{"blocking":false}}'::jsonb WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if idle, stalled, _ := read(); idle != 1 || stalled != 1 {
		t.Fatalf("移管待ちの解除後の配布止まりを表示しなかった idle=%d stalled=%d", idle, stalled)
	}
}
