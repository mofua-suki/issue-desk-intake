package deskcleanup

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"testing"
	"time"
)

// previousVisible reproduces the pre-index archive matching, including legacy projects.
func previousVisible(records []Object, kind string, alerts []any) []any {
	out := []any{}
	for _, raw := range alerts {
		a := fleet.Object(raw)
		target := kind + ":" + AlertID(kind, a)
		fp := AlertFingerprint(a)
		legacy := ""
		if a["project"] == fleet.SystemProject {
			copy := Object{}
			for k, v := range a {
				copy[k] = v
			}
			copy["project"] = fleet.LegacySystemProject
			legacy = AlertFingerprint(copy)
		}
		hidden := false
		for _, r := range records {
			saved := r["fingerprint"]
			if r["snapshot"] != nil {
				saved = AlertFingerprint(fleet.Object(r["snapshot"]))
			}
			if r["target"] == target && (saved == fp || legacy != "" && saved == legacy) {
				hidden = true
				break
			}
		}
		if !hidden {
			out = append(out, raw)
		}
	}
	return out
}
func TestVisibleAlertsMatchesPreviousAlgorithm(t *testing.T) {
	f := setup(t)
	rows := []any{}
	records := []Object{}
	for i := 0; i < 500; i++ {
		a := Object{"key": fmt.Sprint(i), "project": "owner/repo", "kind": "test", "detail": fmt.Sprintf("cause %d", i)}
		rows = append(rows, a)
		if i%2 == 0 {
			records = append(records, Object{"target": "attention:attention:" + fmt.Sprint(i), "snapshot": a})
		} else {
			records = append(records, Object{"target": "unmatched", "snapshot": a})
		}
	}
	old := Object{"key": "legacy", "project": fleet.LegacySystemProject}
	rows = append(rows, Object{"key": "legacy", "project": fleet.SystemProject}, Object{"key": "changed", "project": "owner/repo"})
	records = append(records, Object{"target": "attention:attention:legacy", "fingerprint": AlertFingerprint(old)}, Object{"target": "attention:attention:changed", "fingerprint": "obsolete"}, Object{"target": nil, "snapshot": Object{}})
	data, _ := json.Marshal(records)
	if _, err := f.s.DB.Exec(`INSERT INTO settings(id,json) VALUES('desk-cleanup-archive',?)`, string(data)); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	want := previousVisible(records, "attention", rows)
	oldTime := time.Since(start)
	start = time.Now()
	got, err := VisibleAlerts(t.Context(), f.s.DB, "attention", rows)
	newTime := time.Since(start)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("結果不一致: %v", err)
	}
	t.Logf("旧照合=%s 新照合=%s (各1回、501件超の試験データ)", oldTime, newTime)
}

func TestVisibleAlertsLegacySnapshotMatchesPreviousForBothKinds(t *testing.T) {
	for _, kind := range []string{"attention", "stall"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			legacy := Object{"key": "legacy", "id": "stall-legacy", "project": fleet.LegacySystemProject, "reason": "same"}
			current := Object{"key": "legacy", "id": "stall-legacy", "project": fleet.SystemProject, "reason": "same"}
			changed := Object{"key": "legacy", "id": "stall-legacy", "project": fleet.SystemProject, "reason": "changed"}
			other := Object{"key": "other", "id": "stall-other", "project": fleet.SystemProject, "reason": "same"}
			rows := []any{other, current, changed}
			records := []Object{{"target": kind + ":" + AlertID(kind, current), "fingerprint": "obsolete", "snapshot": legacy}}
			raw, err := json.Marshal(records)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.DB.Exec(`INSERT INTO settings(id,json) VALUES('desk-cleanup-archive',?)`, string(raw)); err != nil {
				t.Fatal(err)
			}
			want := []any{other, changed}
			previous := previousVisible(records, kind, rows)
			got, err := VisibleAlerts(t.Context(), f.s.DB, kind, rows)
			if err != nil || !reflect.DeepEqual(previous, want) || !reflect.DeepEqual(got, want) {
				t.Fatalf("旧=%v 新=%v 期待=%v err=%v", previous, got, want, err)
			}
		})
	}
}
