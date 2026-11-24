package operations

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostdrift"
	"strings"
	"testing"
	"time"
)

func TestPostgresDriftHoldUsesReleaseHistoryAndRepositoryRescue(t *testing.T) {
	s := fixture(t)
	db := postgres(t)
	ctx := context.Background()
	old, newer := strings.Repeat("a", 40), strings.Repeat("b", 40)
	at := s.now().Add(-30 * time.Minute)
	for _, host := range []string{"old", "new"} {
		revision := old
		if host == "new" {
			revision = newer
		}
		if _, e := db.Exec("INSERT INTO hosts(host,revision,heartbeat_at,ready,capacity)VALUES($1,$2,$3,true,1)", host, revision, s.now()); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := db.Exec("INSERT INTO releases(host,flavor,registry,tag,digest,revision,kind,created_at)VALUES('*','','registry',$1,$2,$3,'align',$4)", newer[:12], "sha256:"+strings.Repeat("d", 64), newer, at); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec("INSERT INTO client_release_history(host,flavor,revision,base_revision,kind,created_at)VALUES('*','',$1,$2,'align',$3)", newer, old, at); e != nil {
		t.Fatal(e)
	}
	a := &fleet.Assignments{Hosts: map[string]*fleet.Host{"old": {Max: 1}, "new": {Max: 1}}, Implementers: map[string][]string{"owner/common": {"old", "new"}, "owner/rescue": {"old"}, "owner/rescue2": {"old"}}, Output: map[string]any{}}
	drift := hostdrift.Service{DB: db, Now: s.Now, CompatLimit: 3}
	v, e := drift.Evaluate(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	if _, held := v.Holds["old"]; held || len(v.Allowed["old"]) != 2 {
		t.Fatalf("repo rescue %v", v)
	}
	a.Implementers = map[string][]string{"owner/common": {"old", "new"}}
	v, e = drift.Evaluate(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	if _, held := v.Holds["old"]; !held {
		t.Fatalf("align hold missing %v", v)
	}
	view := map[string]any{"hosts": []any{map[string]any{"name": "old"}}}
	if e = drift.Decorate(ctx, a, view); e != nil {
		t.Fatal(e)
	}
	row := fleet.Object(fleet.List(view["hosts"])[0])
	if fleet.Object(row["driftHold"])["current"] != old || fleet.Object(row["driftHold"])["desired"] != newer {
		t.Fatal("旧Clientの表示欄が欠落", view)
	}
	if _, e = db.Exec("UPDATE client_release_history SET kind='lazy',created_at=$1", s.now().Add(-24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	v, e = drift.Evaluate(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	if len(v.Holds) != 0 {
		t.Fatal("compatible release held")
	}
	if _, e = db.Exec("UPDATE client_release_history SET base_revision=$1", strings.Repeat("c", 40)); e != nil {
		t.Fatal(e)
	}
	v, e = drift.Evaluate(ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	if len(v.Holds) != 0 || len(v.Alerts) == 0 {
		t.Fatal("unknown chain guessed as hold")
	}
}
