package consultations

import (
	"context"
	"database/sql"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/auth"
)

func TestDraftFilingCheckSummaryRead(t *testing.T) {
	s, _, _ := fixture(t)
	checks := []any{}
	for i, result := range []string{"refused", "evidence_requested", "ai_failed", "invalid_answer", "allowed"} {
		checks = append(checks, Record{"revision": 7, "phase": i, "at": "2026-11-09T00:00:00Z", "result": result, "reason": fmt.Sprintf("理由%d", i+1), "inputOmissions": []any{Record{"candidateId": "内部の省略記録"}}, "comparisonId": "internal"})
	}
	d := Record{"id": todoID, "status": "submit_error", "filingCheckCalls": Record{"revision": 7, "count": 5, "checks": checks, "lastRefusal": "内部の拒否"}}
	view := s.DraftView(d, auth.Principal{Kind: auth.Screen}, "http://desk.test")
	if _, leaked := view["filingCheckCalls"]; leaked {
		t.Fatal("内部の履歴を公開しています")
	}
	summary := record(view["filingCheckSummary"])
	if summary["revision"] != 7 || summary["count"] != 5 || len(fleet.Items(summary["checks"])) != 5 {
		t.Fatalf("%+v", summary)
	}
	for i, item := range fleet.Items(summary["checks"]) {
		row := record(item)
		if row["reason"] != fmt.Sprintf("理由%d", i+1) || row["result"] != text(record(checks[i])["result"]) || row["inputOmissions"] != nil || row["comparisonId"] != nil {
			t.Fatalf("%+v", row)
		}
	}
	if strings.Contains(fmt.Sprint(summary), "内部") {
		t.Fatalf("内部の項目が出ています: %+v", summary)
	}
}

func TestDraftFilingCheckSummaryRedactionAndLegacy(t *testing.T) {
	s, _, _ := fixture(t)
	p := auth.Principal{Kind: auth.Screen}
	if _, found := s.DraftView(Record{"id": todoID, "status": "draft"}, p, "")["filingCheckSummary"]; found {
		t.Fatal("記録の無い相談には要約を出さない")
	}
	legacy := Record{"id": todoID, "status": "draft", "filingCheckCalls": Record{"lastRefusal": "過去の拒否", "checks": []any{Record{"phase": 0}}}}
	summary := record(s.DraftView(legacy, p, "")["filingCheckSummary"])
	row := record(fleet.Items(summary["checks"])[0])
	if summary["count"] != nil || summary["revision"] != nil || row["result"] != nil || row["reason"] != nil {
		t.Fatalf("旧記録の欠けは未記録のまま: %+v", summary)
	}
	if record(legacy["filingCheckCalls"])["lastRefusal"] != "過去の拒否" || legacy["filingCheckSummary"] != nil {
		t.Fatal("読取で保存記録を変えています")
	}
	rows := []any{}
	for i := 0; i < 21; i++ {
		rows = append(rows, Record{"result": "refused", "reason": fmt.Sprintf("理由%d", i)})
	}
	rows[20] = Record{"result": "秘密の種類", "reason": "token: abcdefghijkl1234\n二行目" + strings.Repeat("長", 900)}
	kept := fleet.Items(record(s.DraftView(Record{"id": todoID, "status": "draft", "filingCheckCalls": Record{"checks": rows}}, p, "")["filingCheckSummary"])["checks"])
	if len(kept) != 20 || record(kept[0])["reason"] != "理由1" {
		t.Fatalf("%d %+v", len(kept), kept[0])
	}
	last := record(kept[19])
	reason := text(last["reason"])
	if last["result"] != nil || strings.Contains(reason, "abcdefghijkl") || strings.Contains(reason, "\n") || len([]rune(reason)) > 501 {
		t.Fatalf("%+v", last)
	}
}

func TestDraftFilingCheckSummaryOwnerBoundary(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	if _, err := s.options.Records.Change(ctx, "drafts", todoID, func(*sql.Tx, Record) (Record, error) {
		return Record{"id": todoID, "owner": "someone-else", "project": "owner/app", "status": "draft", "filingCheckCalls": Record{"checks": []any{}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Draft(ctx, todoID, auth.Principal{Kind: auth.PC, Name: "other"}); err == nil || !strings.Contains(err.Error(), "見つかりません") {
		t.Fatalf("他の所有者の下書きは読めない: %v", err)
	}
}
