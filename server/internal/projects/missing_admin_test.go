package projects

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"testing"
)

func TestRegisteredRecoveryWithoutAdminDoesNotUseNilAPI(t *testing.T) {
	f := newFixture(t)
	f.s.o.Gitea = nil
	f.s.o.Repositories = func() []string { return []string{"owner/demo"} }
	d := definition()
	d["placement"] = "a9max"
	p := Object{"id": "11111111-1111-4111-8111-111111111111", "status": "registered", "definition": d, "approval": Object{"fingerprint": Fingerprint(d)}, "creation": Object{"steps": Object{"registered": Object{"name": "owner/demo"}, "created": Object{"branch": "main"}}}, "filing": Object{"draft": "22222222-2222-4222-8222-222222222222"}}
	if e := f.s.save(context.Background(), p); e != nil {
		t.Fatal(e)
	}
	if e := f.s.file(context.Background(), fleet.Text(p["id"])); e == nil {
		t.Fatal("管理接続なしで照合を続行しました")
	}
	registration := NewRegistration(RegistrationOptions{})
	if e := registration.Preflight(context.Background(), d, Object{}); e == nil {
		t.Fatal("管理接続なしで登録を続行しました")
	}
}
