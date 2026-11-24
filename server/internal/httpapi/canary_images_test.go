package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
	"issue-desk-intake/server/internal/hosts"
)

func TestCanaryDesiredMatchesHostsView(t *testing.T) {
	for _, flavor := range []string{"", "-python"} {
		t.Run("flavor"+flavor, func(t *testing.T) {
			for _, tc := range []struct {
				name, status, host, override            string
				changedGlobal                           bool
				wantRevision, wantSource, wantImageRole string
				wantRolloutRole                         any
			}{
				{name: "global_canary", status: "canary", host: "nas", wantRevision: "b", wantSource: "global", wantImageRole: "canary", wantRolloutRole: "canary"},
				{name: "global_held", status: "canary", host: "other", wantRevision: "a", wantSource: "global", wantImageRole: "previous", wantRolloutRole: "held"},
				{name: "global_passed", status: "passed", host: "other", wantRevision: "b", wantSource: "global", wantImageRole: "desired", wantRolloutRole: "spread"},
				{name: "global_failed", status: "failed", host: "other", wantRevision: "a", wantSource: "global", wantImageRole: "previous", wantRolloutRole: "held"},
				{name: "override_canary", status: "canary", host: "nas", override: "c", wantRevision: "c", wantSource: "host", wantImageRole: "desired"},
				{name: "override_same_revision", status: "canary", host: "nas", override: "b", wantRevision: "b", wantSource: "host", wantImageRole: "desired"},
				{name: "override_held", status: "canary", host: "other", override: "c", wantRevision: "c", wantSource: "host", wantImageRole: "desired"},
				{name: "override_passed", status: "passed", host: "other", override: "c", wantRevision: "c", wantSource: "host", wantImageRole: "desired"},
				{name: "override_failed", status: "failed", host: "nas", override: "c", wantRevision: "c", wantSource: "host", wantImageRole: "desired"},
				{name: "different_global", status: "canary", host: "other", changedGlobal: true, wantRevision: "d", wantSource: "global", wantImageRole: "desired"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f := newCanaryFlavorFixture(t, flavor)
					f.exec(t, `INSERT INTO hosts(host,heartbeat_at,hostd_health) VALUES('other',$1,$2::jsonb)`, f.now, `{"cycle":{"versions":{"current":"`+f.tag(f.old)+`"}}}`)
					f.exec(t, `UPDATE hosts SET hostd_health=$1::jsonb WHERE host='nas'`, `{"cycle":{"versions":{"current":"`+f.tag(f.old)+`"}}}`)
					f.exec(t, `UPDATE release_rollouts SET status=$2 WHERE id=$1`, f.id, tc.status)
					if tc.override != "" {
						f.register(t, tc.host, strings.Repeat(tc.override, 40))
					}
					if tc.changedGlobal {
						f.exec(t, `UPDATE releases SET revision=$1,tag=$2 WHERE host='*' AND flavor=$3`, strings.Repeat("d", 40), f.tag(strings.Repeat("d", 40)), flavor)
					}
					// Compare the exact same persisted state. Reading View must never decide it,
					// even when the next maintenance/request evaluation would end the canary.
					before := f.row(t)
					a := &fleet.Assignments{Output: map[string]any{}}
					svc := hosts.Service{DB: f.db, Now: func() time.Time { return f.now }}
					view, err := svc.View(context.Background(), a)
					if err != nil {
						t.Fatal(err)
					}
					if before != f.row(t) {
						t.Fatal("View mutated rollout")
					}
					var target map[string]any
					for _, raw := range fleet.List(fleet.Object(view)["hosts"]) {
						row := fleet.Object(raw)
						if row["name"] == tc.host {
							target = row
						}
					}
					if target == nil {
						t.Fatal("host absent", view)
					}
					image := fleet.Object(fleet.Object(target["image"])["desired"])
					if image["revision"] != strings.Repeat(tc.wantRevision, 40) || image["source"] != tc.wantSource || image["role"] != tc.wantImageRole {
						t.Fatal("desired display", image)
					}
					rollout := fleet.Object(target["rollout"])
					if rollout["role"] != tc.wantRolloutRole || rollout["stage"] != tc.status || rollout["revision"] != f.revision {
						t.Fatal("global progress", rollout)
					}
					if tc.override != "" && !strings.HasPrefix(fleet.Text(rollout["reason"]), "host_override") {
						t.Fatal("override reason missing", rollout)
					}
					if len(fleet.List(fleet.Object(view)["releaseRollouts"])) != 1 {
						t.Fatal("global progress missing", view)
					}
					result, err := f.service.Desired(context.Background(), tc.host, flavor, f.old)
					if err != nil {
						t.Fatal(err)
					}
					desired := fleet.Object(result)["desired"].(*hostd.Release)
					source := "host"
					if desired.Host == "*" {
						source = "global"
					} else if desired.Host != tc.host {
						t.Fatal("unexpected desired.host", desired.Host)
					}
					if desired.Revision != image["revision"] || desired.Tag != image["tag"] || source != image["source"] {
						t.Fatal("Desired/View differ", desired, image)
					}
				})
			}
		})
	}
}

func TestCanaryHostsViewDoesNotEvaluateExpiredRollout(t *testing.T) {
	f := newCanaryFixture(t)
	f.register(t, "nas", strings.Repeat("c", 40))
	f.now = f.started.Add(4 * time.Hour)
	before := f.row(t)
	svc := hosts.Service{DB: f.db, Now: func() time.Time { return f.now }}
	result, err := svc.View(context.Background(), &fleet.Assignments{Output: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if before != f.row(t) || !strings.Contains(string(raw), `"stage":"canary"`) {
		t.Fatal("reading view evaluated expiry", string(raw))
	}
}

func TestCanaryDesiredReadsSavedDecisionUntilTick(t *testing.T) {
	f := newCanaryFixture(t)
	f.now = f.started.Add(180 * time.Minute)
	f.exec(t, `UPDATE hosts SET heartbeat_at=$1`, f.now)
	f.cycle(t, "third")
	f.exec(t, `UPDATE release_rollouts SET success_cycles=2 WHERE id=$1`, f.id)
	before := f.row(t)
	result, err := f.service.Desired(context.Background(), "nas", "", f.old)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Object(result)["desired"].(*hostd.Release).Revision != f.revision || before != f.row(t) {
		t.Fatal("Desired changed evaluation", result)
	}
	svc := hosts.Service{DB: f.db, Now: func() time.Time { return f.now }}
	view, err := svc.View(context.Background(), &fleet.Assignments{Output: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	row := fleet.Object(fleet.List(fleet.Object(view)["hosts"])[0])
	if fleet.Object(fleet.Object(row["image"])["desired"])["revision"] != f.revision {
		t.Fatal("View differs before Tick", view)
	}
	f.tick(t)
	f.want(t, "failed", "timeout", 2)
	result, err = f.service.Desired(context.Background(), "nas", "", f.old)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Object(result)["desired"].(*hostd.Release).Revision != f.old {
		t.Fatal("Desired ignored Tick decision", result)
	}
	view, err = svc.View(context.Background(), &fleet.Assignments{Output: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	row = fleet.Object(fleet.List(fleet.Object(view)["hosts"])[0])
	if fleet.Object(fleet.Object(row["image"])["desired"])["revision"] != f.old {
		t.Fatal("View differs after Tick", view)
	}
}
