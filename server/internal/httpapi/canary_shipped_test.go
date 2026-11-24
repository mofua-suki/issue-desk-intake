package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
	"issue-desk-intake/server/internal/hosts"
	"issue-desk-intake/server/internal/operations"
)

func TestCanaryShippedConfigurationMaintenance(t *testing.T) {
	// Use the files shipped to NAS, not a test-only assignment/deployment policy.
	root := filepath.Join("..", "..", "..")
	config := fleet.Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}
	_, a, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var deployment struct {
		Flavor string            `json:"flavor"`
		Hosts  map[string]string `json:"hosts"`
	}
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "nas", "worker-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &deployment); err != nil {
		t.Fatal(err)
	}
	if len(a.ReleaseCanaryHosts) == 0 {
		t.Fatal("shipped canary candidates absent")
	}
	host := a.ReleaseCanaryHosts[0]
	registry := deployment.Hosts[host]
	if registry == "" {
		t.Fatal("canary not deployed by shipped release configuration")
	}
	_, db, _ := workerFixture(t)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	service := hostd.Service{DB: db, Now: func() time.Time { return now }, Assignments: func() (*fleet.Assignments, error) { return a, nil }}
	old, next, override := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	tag := func(revision string) string {
		value := revision[:12]
		if deployment.Flavor == "-python" {
			value += "-client-python"
		}
		return value
	}
	register := func(host, revision string) {
		t.Helper()
		_, err := service.Register(context.Background(), map[string]any{"host": host, "flavor": deployment.Flavor, "registry": registry, "tag": tag(revision), "digest": "sha256:" + strings.Repeat("f", 64), "revision": revision})
		if err != nil {
			t.Fatal(err)
		}
	}
	register("*", old)
	if _, err = db.Exec(`INSERT INTO hosts(host,heartbeat_at,ready,image_revision,hostd_health) VALUES($1,$2,true,$3,$4::jsonb)`, host, now, tag(old), `{"cycle":{"versions":{"current":"`+tag(old)+`"}}}`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO host_image_results(host,tag,flavor,digest,status) VALUES($1,$2,$3,$4,'success')`, host, tag(old), deployment.Flavor, "sha256:"+strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	register("*", next)
	register(host, override)
	started := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if _, err = db.Exec(`UPDATE release_rollouts SET started_at=$1`, started); err != nil {
		t.Fatal(err)
	}
	// This is the same entry called by startup's ordinary 30-second maintenance.
	err = WorkerMaintenance(context.Background(), WorkerRouteOptions{DB: db, FleetPath: config.FleetPath, AssignmentsPath: config.AssignmentsPath, Now: service.Now})
	if err != nil {
		t.Fatal(err)
	}
	var status, reason, canary string
	var savedStart time.Time
	var cycles int
	if err = db.QueryRow(`SELECT status,reason,canary,started_at,success_cycles FROM release_rollouts`).Scan(&status, &reason, &canary, &savedStart, &cycles); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "timeout:host_override" || canary != host || cycles != 0 || !savedStart.Equal(started) {
		t.Fatal(status, reason, canary, cycles, savedStart)
	}
	alerts, err := (&operations.Service{PostgreSQL: db, Now: service.Now, Assignments: a}).Stalls(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, value := range alerts {
		alert := fleet.Object(value)
		if alert["kind"] == "hostd_rollback" && alert["host"] == host && strings.Contains(fleet.Text(alert["detail"]), "timeout:host_override") {
			found = true
		}
	}
	if !found {
		t.Fatal("canary failure alert missing", alerts)
	}
	desired, err := service.Desired(context.Background(), host, deployment.Flavor, old)
	if err != nil {
		t.Fatal(err)
	}
	release := fleet.Object(desired)["desired"].(*hostd.Release)
	view, err := (&hosts.Service{DB: db, Now: service.Now}).View(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range fleet.List(fleet.Object(view)["hosts"]) {
		row := fleet.Object(raw)
		if row["name"] == host {
			image := fleet.Object(fleet.Object(row["image"])["desired"])
			rollout := fleet.Object(row["rollout"])
			if release.Host != host || image["source"] != "host" || image["revision"] != release.Revision || image["tag"] != release.Tag || rollout["stage"] != "failed" || rollout["role"] != nil {
				t.Fatal(release, image, rollout)
			}
			return
		}
	}
	t.Fatal("shipped host absent from View")
}
