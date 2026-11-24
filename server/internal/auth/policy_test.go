package auth

import (
	"testing"
	"time"
)

// 利用者だけが出せる承認は、画面か、運用の代行を許したトークンが利用者の具体的な発言と指示の日時を添えたときだけ通す。
func TestUserApprovalNeedsScreenOrDelegatedStatement(t *testing.T) {
	now := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	statement, at := "共通番号12の操作を承認します", "2026-11-11T11:58:00+09:00"
	if got, p := UserApproval(Principal{Kind: Screen}, nil, nil, now, "操作"); p != nil || got != (ChatApproval{}) {
		t.Fatal(got, p)
	}
	delegated := Principal{Kind: PC, Name: "nas", Delegated: true}
	for _, c := range []struct {
		p         Principal
		statement any
		at        any
		status    int
	}{
		{Principal{Kind: CLI}, statement, at, 403},
		{Principal{Kind: Hostd, Name: "nas"}, statement, at, 403},
		{Principal{Kind: PC, Name: "pc"}, statement, at, 403},
		{delegated, nil, at, 400},
		{delegated, "承認します", at, 400},
		{delegated, "よろしくお願いいたします。", at, 400},
		{delegated, "短い", at, 400},
		{delegated, 12345678, at, 400},
		// 指示の日時が無い・オフセットが無い・未来・7日より前は断る。
		{delegated, statement, nil, 400},
		{delegated, statement, "", 400},
		{delegated, statement, "2026-11-11T11:58:00", 400},
		{delegated, statement, "2026-11-11T12:01:00+09:00", 400},
		{delegated, statement, "2026-11-04T11:59:00+09:00", 400},
	} {
		if _, p := UserApproval(c.p, c.statement, c.at, now, "操作"); p == nil || p.Status != c.status {
			t.Fatalf("%v %v %v: %v", c.p, c.statement, c.at, p)
		}
	}
	got, p := UserApproval(delegated, "  "+statement+" ", at, now, "操作")
	if p != nil || got != (ChatApproval{Statement: statement, InstructionAt: at}) {
		t.Fatal(got, p)
	}
}
