package hosts

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

// legacyImageResults reproduces the original per-host and per-result scans.
func legacyImageResults(rows []map[string]any, host, flavor string) ([]any, []any) {
	failed, shown := []any{}, []any{}
	for _, r := range rows {
		if r["host"] != host {
			continue
		}
		if r["status"] == "failed" && r["flavor"] == flavor {
			failed = append(failed, r["tag"])
		}
		if len(shown) >= 5 {
			continue
		}
		succeeded, seen := []string{}, map[string]bool{}
		for _, other := range rows {
			if other["host"] != host && other["tag"] == r["tag"] && other["flavor"] == r["flavor"] && other["status"] == "success" {
				name := other["host"].(string)
				if !seen[name] {
					seen[name] = true
					succeeded = append(succeeded, name)
				}
			}
		}
		sort.Strings(succeeded)
		shown = append(shown, map[string]any{"tag": r["tag"], "flavor": r["flavor"], "status": r["status"], "at": r["at"], "succeededOn": succeeded})
	}
	return failed, shown
}

// Rows have the database's host/tag/flavor uniqueness and descending read order,
// including equal timestamps. The same tag succeeds on multiple hosts.
func imageFixture(hosts, tags int) []map[string]any {
	rows := []map[string]any{}
	for tag := tags - 1; tag >= 0; tag-- {
		for h := 0; h < hosts; h++ {
			for _, flavor := range []string{"", "-python"} {
				status := "success"
				if (tag+h)%3 == 0 {
					status = "failed"
				}
				rows = append(rows, map[string]any{"host": fmt.Sprintf("host-%03d", h), "tag": fmt.Sprintf("%012x", tag), "flavor": flavor, "status": status, "at": fmt.Sprintf("2026-11-%02dT00:00:00Z", 1+tag/100)})
			}
		}
	}
	return rows
}

func TestImageResultIndexEquivalent(t *testing.T) {
	for _, rows := range [][]map[string]any{nil, imageFixture(24, 150)} {
		idx := indexImageResults(rows)
		for h := 0; h < 25; h++ {
			host := fmt.Sprintf("host-%03d", h)
			for _, flavor := range []string{"", "-python"} {
				oldFailed, oldShown := legacyImageResults(rows, host, flavor)
				failed, shown := idx.forHost(host, flavor)
				oldJSON, err := json.Marshal([]any{oldFailed, oldShown})
				if err != nil {
					t.Fatal(err)
				}
				newJSON, err := json.Marshal([]any{failed, shown})
				if err != nil {
					t.Fatal(err)
				}
				if string(oldJSON) != string(newJSON) {
					t.Fatalf("%s %q: output differs\nold %s\nnew %s", host, flavor, oldJSON, newJSON)
				}
				if len(rows) > 0 && h < 24 && (len(failed) <= 5 || len(shown) != 5) {
					t.Fatalf("fixture did not exercise retained failures: %s", host)
				}
			}
		}
	}
}

func BenchmarkImageResults(b *testing.B) {
	const hosts, tags = 100, 100
	rows := imageFixture(hosts, tags)
	b.Run("legacy", func(b *testing.B) {
		b.ReportAllocs()
		// Each host scans every row once, and each of its five displayed rows scans it again.
		b.ReportMetric(float64(hosts*6*len(rows)), "row-visits/op")
		for i := 0; i < b.N; i++ {
			for h := 0; h < hosts; h++ {
				legacyImageResults(rows, fmt.Sprintf("host-%03d", h), "")
			}
		}
	})
	b.Run("indexed", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(float64(len(rows)), "index-row-visits/op")
		for i := 0; i < b.N; i++ {
			idx := indexImageResults(rows)
			for h := 0; h < hosts; h++ {
				idx.forHost(fmt.Sprintf("host-%03d", h), "")
			}
		}
	})
}
