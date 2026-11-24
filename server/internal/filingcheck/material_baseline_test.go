package filingcheck

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"sort"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/pgtest"
)

// readMaterialDrafts65c7199 reproduces the pre-change read algorithm recorded
// for 65c7199a3acefb3e3d3324ef99150ba41aab8ed0: unbounded, cycle-stopping
// ancestors plus independent candidates, with DB keys and ReadFilingSnapshot.
// It deliberately does not include the subsequently added sibling selection.
// The snapshot implementation's SHA-256 matches that ref (see measurement doc).
// Historical source access is controller-owned: the selector's exact source
// identity must be checked there, rather than inferred from this reconstruction.
func readMaterialDrafts65c7199(ctx context.Context, reader materialDraftReader, d Record) ([]Record, error) {
	return reader.ReadFilingSnapshot(ctx, func(entries []consultations.DraftFields) []string {
		drafts := make([]Record, len(entries))
		byID := map[string]Record{}
		for i, entry := range entries {
			drafts[i] = entry.Record
			byID[fleet.Text(entry.Record["id"])] = entry.Record
		}
		family := map[string]bool{}
		for _, member := range baselineFamilyMembers(d, drafts) {
			family[fleet.Text(member["id"])] = true
		}
		ancestors := map[string]bool{}
		for id := baselineParent(d); id != "" && !ancestors[id]; id = baselineParent(byID[id]) {
			ancestors[id] = true
		}
		comparisonAt := baselineComparisonAt(d, drafts)
		keys := []string{}
		for _, entry := range entries {
			row := entry.Record
			if ancestors[fleet.Text(row["id"])] || baselineCandidate(d, row, family, comparisonAt) {
				keys = append(keys, entry.Key)
			}
		}
		return keys
	})
}

func baselineComparisonAt(d Record, drafts []Record) string {
	comparisonAt := fleet.Text(d["createdAt"])
	if p := fleet.Text(d["parentSplit"]); p != "" {
		for _, row := range drafts {
			if row["id"] == p {
				return fleet.Text(row["createdAt"])
			}
		}
	}
	return comparisonAt
}

func baselineCandidate(d, other Record, familyID map[string]bool, comparisonAt string) bool {
	id := fleet.Text(other["id"])
	created := fleet.Text(other["createdAt"])
	return id != fleet.Text(d["id"]) && !familyID[id] && strings.EqualFold(fleet.Text(other["project"]), fleet.Text(d["project"])) && other["issue"] == nil && len(fleet.Elements(other["issues"])) == 0 && other["parentSplit"] == nil && baselineActive(other) && (created < comparisonAt || created == comparisonAt && id < fleet.Text(d["id"]))
}

func baselineActive(d Record) bool {
	switch fleet.Text(d["status"]) {
	case "thinking", "draft", "auto_ready", "submitting", "submit_unknown", "submit_error", "intake_waiting":
		return true
	}
	return false
}

func baselineParent(d Record) string {
	if p := fleet.Text(d["parentSplit"]); p != "" {
		return p
	}
	return fleet.Text(d["parentConsultation"])
}

// Keep the historical family traversal and allocations in the measured control.
func baselineRootID(d Record, byID map[string]Record) string {
	seen := map[string]int{}
	visited := []string{}
	for i := 0; i < 64; i++ {
		id := fleet.Text(d["id"])
		if position, exists := seen[id]; exists {
			cycle := append([]string{}, visited[position:]...)
			sort.Strings(cycle)
			return cycle[0]
		}
		seen[id] = len(visited)
		visited = append(visited, id)
		p := baselineParent(d)
		if p == "" {
			return id
		}
		next := byID[p]
		if next == nil {
			return p
		}
		d = next
	}
	return ""
}
func baselineFamilyMembers(d Record, drafts []Record) []Record {
	if baselineParent(d) == "" {
		return nil
	}
	byID := map[string]Record{}
	for _, item := range drafts {
		byID[fleet.Text(item["id"])] = item
	}
	byID[fleet.Text(d["id"])] = d
	root := baselineRootID(d, byID)
	if root == "" {
		return nil
	}
	out := []Record{}
	for _, item := range drafts {
		if baselineRootID(item, byID) == root {
			out = append(out, item)
		}
	}
	return out
}

// Prove the measurement control retains the old over-read at the same
// boundaries, rather than accidentally benchmarking the optimized selector twice.
func TestMaterialBaselineRetainsUnboundedAncestors(t *testing.T) {
	env := pgtest.NewIsolated(t)
	records, err := consultations.OpenRecords(t.Context(), env.DB)
	if err != nil {
		t.Fatal(err)
	}
	observed, counts := observedMaterialRecords(t, env)
	for _, tc := range ancestorBoundaries() {
		if tc.siblings {
			continue
		} // siblings were added after this baseline.
		t.Run(tc.name, func(t *testing.T) {
			target, _ := seedAncestorBoundary(t, records, tc)
			*counts = materialObservation{}
			got, err := readMaterialDrafts65c7199(t.Context(), observed, target)
			if err != nil {
				t.Fatal(err)
			}
			want := tc.count
			if tc.end == "target" {
				want++
			}
			if counts.Selected.Rows != want || counts.Fallback.Calls != 0 || counts.Commits != 1 {
				t.Fatalf("baseline changed: %+v want rows=%d", counts, want)
			}
			legacy, err := records.List(t.Context(), "drafts")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decisionContext(target, got), decisionContext(target, legacy)) {
				t.Fatal("baseline context differs")
			}
		})
	}
}
