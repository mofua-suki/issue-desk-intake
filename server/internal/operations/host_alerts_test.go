package operations

import (
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"
)

func TestPostgresHostAlertsNativeTestOnly(t *testing.T) {
	s := fixture(t)
	s.PostgreSQL = postgres(t)
	s.Assignments = &fleet.Assignments{Hosts: map[string]*fleet.Host{}, Output: map[string]any{}}
	now := s.now()
	stamp := now.UnixMilli()
	cases := []struct {
		host, role, capabilities string
		health                   any
	}{
		{"runner", "test-only", `{"nativeTest":true,"platform":"darwin"}`, nil},
		{"normal", "implementation", `{"nativeTest":true,"platform":"darwin"}`, nil},
		{"role-only", "test-only", `{}`, nil},
		{"missing-native", "test-only", `{"platform":"darwin"}`, nil},
		{"missing-platform", "test-only", `{"nativeTest":true}`, nil},
		{"wrong-native", "test-only", `{"nativeTest":"true","platform":"darwin"}`, nil},
		{"false-native", "test-only", `{"nativeTest":false,"platform":"darwin"}`, nil},
		{"wrong-platform", "test-only", `{"nativeTest":true,"platform":"linux"}`, nil},
		{"bad-platform", "test-only", `{"nativeTest":true,"platform":["darwin"]}`, nil},
		{"failed", "test-only", `{"nativeTest":true,"platform":"darwin"}`, map[string]any{"cycle": map[string]any{"result": "failed", "stage": "docker", "at": iso(now), "receivedAt": stamp, "firstFailureAt": stamp - 31*60000, "failures": 3}}},
		{"wait", "test-only", `{"nativeTest":true,"platform":"darwin"}`, map[string]any{"cycle": map[string]any{"id": "0123456789abcdef0123456789abcdef", "phase": "wait", "result": "success", "stage": "swap-start", "waitingFor": "drain", "at": iso(now), "receivedAt": stamp, "lastSuccessAt": stamp, "failures": 0}}},
	}
	for _, tc := range cases {
		s.Assignments.Hosts[tc.host] = &fleet.Host{Role: tc.role}
		health, err := json.Marshal(tc.health)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.PostgreSQL.Exec(`INSERT INTO hosts(host, heartbeat_at, ready, capabilities, hostd_health) VALUES($1,$2,true,$3::jsonb,$4::jsonb)`, tc.host, now, tc.capabilities, string(health))
		if err != nil {
			t.Fatal(err)
		}
	}
	alerts, err := s.hostAlerts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, alert := range alerts {
		got[fleet.Text(fleet.Object(alert)["id"])]++
	}
	want := map[string]int{"hostd-silent-normal": 1, "hostd-silent-role-only": 1, "hostd-silent-missing-native": 1, "hostd-silent-missing-platform": 1, "hostd-silent-wrong-native": 1, "hostd-silent-false-native": 1, "hostd-silent-wrong-platform": 1, "hostd-silent-bad-platform": 1, "hostd-stalled-failed": 1, "hostd-wait-wait": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("alerts: got %v, want %v", got, want)
	}
}
