package operations

import (
	"strings"
	"testing"
	"time"
)

func TestDispatchHostReasonsReportedCapacity(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name                               string
		implementing, publishing, capacity any
		max                                int
		full                               bool
	}{
		{"publishing fills capacity", float64(0), float64(4), float64(4), 8, true},
		{"configured limit lower", float64(1), float64(1), float64(4), 2, true},
		{"capacity lower", float64(1), float64(1), float64(2), 4, true},
		{"missing capacity uses configured", float64(1), float64(1), nil, 4, false},
		{"missing publishing", float64(4), nil, float64(4), 4, false},
		{"invalid capacity ignored", float64(1), float64(1), float64(-1), 4, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := map[string]any{"implementing": tc.implementing, "publishing": tc.publishing, "capacity": tc.capacity, "maxImplementations": tc.max, "ready": true, "updatedAt": iso(now)}
			detail := strings.Join(dispatchHostReasons(row, now, 20), "\n")
			if strings.Contains(detail, "報告値では枠が埋まっている") != tc.full || strings.Contains(detail, "受け取り可と報告") {
				t.Fatal(detail)
			}
		})
	}
}

func TestDispatchDrainOrigin(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, by, want string }{
		{"hostd", "hostd:gt12", "hostd の更新待ちの drain"},
		{"manual", "operator", "手動の drain"},
		{"legacy", "", "手動の drain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drain := map[string]any{"at": "start", "reason": "client image update v123", "until": "end"}
			if tc.by != "" {
				drain["by"] = tc.by
			}
			row := map[string]any{"drain": drain, "ready": true, "updatedAt": iso(now)}
			detail := strings.Join(dispatchHostReasons(row, now, 20), "\n")
			for _, want := range []string{tc.want, "client image update v123", "期限: end"} {
				if !strings.Contains(detail, want) {
					t.Fatalf("missing %q in %q", want, detail)
				}
			}
			if tc.name == "hostd" && strings.Contains(detail, "手動の drain") {
				t.Fatal(detail)
			}
		})
	}
}

func TestDispatchIdleShowsOnlyAssignedHosts(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	available := []map[string]any{{"repo": "INTAKE"}, {"repo": "worker"}, {"repo": "intake"}}
	assignments := map[string][]string{"intake": {"gt12", "nab-9s"}, "worker": {"nab-9s"}, "other": {"a9max"}}
	rows := map[string]map[string]any{
		"gt12":  {"drain": map[string]any{"by": "hostd:gt12", "reason": "update"}, "ready": true, "updatedAt": iso(now)},
		"a9max": {"ready": true, "updatedAt": iso(now)},
	}
	detail := strings.Join(dispatchIdleHostDetails(available, assignments, rows, now, 20), "\n")
	for _, want := range []string{"gt12: hostd の更新待ちの drain", "nab-9s: 状態の報告なし"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("missing %q in %q", want, detail)
		}
	}
	if strings.Contains(detail, "a9max") || strings.Count(detail, "gt12:") != 1 || strings.Count(detail, "nab-9s:") != 1 {
		t.Fatal(detail)
	}
}

func TestDispatchDrainDeadline(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		drain map[string]any
		want  string
	}{
		{"indefinite", map[string]any{"indefinite": true}, "無期限"},
		{"dated", map[string]any{"until": "2026-11-07T12:00:00Z"}, "2026-11-07T12:00:00Z"},
		{"missing", map[string]any{}, "不明"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := map[string]any{"drain": tc.drain, "ready": true, "updatedAt": iso(now)}
			detail := strings.Join(dispatchHostReasons(row, now, 20), "\n")
			if !strings.Contains(detail, "期限: "+tc.want) {
				t.Fatal(detail)
			}
		})
	}
}
