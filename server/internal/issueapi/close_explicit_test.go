package issueapi

import (
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"testing"
)

func TestExplicitCloseUsesExistingNonForceWorkflowPostgres(t *testing.T) {
	f := newFixture(t)
	p := auth.Principal{Kind: auth.PC, Name: "a", Delegated: true}
	detail, e := f.s.Detail(f.ctx, "owner/repo", 1, false, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.CloseExplicit(f.ctx, "owner/repo", 1, "利用者が明示して終了", fleet.Text(detail["updatedAt"]), auth.Principal{Kind: auth.CLI}); e == nil {
		t.Fatal("CLIを受理")
	}
	if _, e = f.s.CloseExplicit(f.ctx, "owner/repo", 1, "利用者が明示して終了", "old", p); e == nil {
		t.Fatal("古い対象を受理")
	}
	if _, e = f.s.CloseExplicit(f.ctx, "owner/repo", 1, "利用者が明示して終了", fleet.Text(detail["updatedAt"]), p); e != nil {
		t.Fatal(e)
	}
	job, e := f.options.Jobs.Get(f.ctx, f.job)
	if e != nil || job.Status != "closed" || f.api.closed != 1 {
		t.Fatal("終了未保存", job, e)
	}
}

func TestExplicitCloseAuditRetainsActualCallerPostgres(t *testing.T) {
	for _, kind := range []auth.Kind{auth.PC, auth.Screen} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t)
			detail, e := f.s.Detail(f.ctx, "owner/repo", 1, false, false)
			if e != nil {
				t.Fatal(e)
			}
			p := auth.Principal{Kind: kind, Name: "delegate", Delegated: true}
			if _, e = f.s.CloseExplicit(f.ctx, "owner/repo", 1, "本人の指示で終了", fleet.Text(detail["updatedAt"]), p); e != nil {
				t.Fatal(e)
			}
			records, e := f.s.closeRecords(f.ctx, closeHistoryFilter("owner/repo"))
			if e != nil || len(records) != 1 {
				t.Fatal(records, e)
			}
			decision := fleet.ObjectOrEmpty(records[0]["decision"])
			want, description := "screen", "画面から直接終了"
			if kind == auth.PC {
				want, description = "explicit", "利用者の明示指示で終了"
			}
			if decision["method"] != want || decision["via"] != via(p) || records[0]["owner"] != via(p) || records[0]["description"] != description {
				t.Fatal(records[0])
			}
		})
	}
}
