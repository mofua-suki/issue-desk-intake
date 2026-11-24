package filingcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/pgtest"
)

// Explicit opt-in; ordinary regression runs must not prepare this large dataset.
func TestMeasureMaterialDrafts(t *testing.T) {
	if os.Getenv("ISSUE_DESK_MEASURE_MATERIAL") != "1" {
		t.Skip("set ISSUE_DESK_MEASURE_MATERIAL=1 for dedicated synthetic measurement")
	}
	env := pgtest.NewIsolated(t)
	records, err := consultations.OpenRecords(t.Context(), env.DB)
	if err != nil {
		t.Fatal(err)
	}
	observed, counts := observedMaterialRecords(t, env)
	var version string
	if err = env.DB.QueryRowContext(t.Context(), "SELECT version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("environment go=%s os=%s arch=%s cpus=%d postgres=%s", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), version)
	t.Log("synthetic only; 7687 rows, target saved UTF-8 bytes=362545211; 256 ancestors; 3 repetitions; one warmup per variant/path; alternating baseline/after order; no cache flush; preparation excluded; total includes pool acquisition, hold starts after BeginTx success and ends after Commit/Rollback; returned bytes=sum string/[]byte lengths including DB keys, excludes protocol/storage overhead")
	variants := []struct {
		name string
		read func(context.Context, materialDraftReader, Record) ([]Record, error)
	}{{"65c7199-reconstructed", readMaterialDrafts65c7199}, {"after", readMaterialDrafts}}
	for _, percent := range []int{1, 10, 50} {
		target := seedMaterialMeasurement(t, records, percent)
		for _, fallback := range []bool{false, true} {
			// Change only a non-projected field in an existing row: row count
			// and eligibility stay fixed. An escaped NUL in a key triggers 22P05.
			replacement := `"title":"ordinary"`
			if fallback {
				replacement = `"\u0000":"ordinary"`
			}
			if _, err = env.DB.ExecContext(t.Context(), `UPDATE drafts SET json=replace(json,'"title":"ordinary"',$1) WHERE id='db-m07686'`, replacement); err != nil {
				t.Fatal(err)
			}
			path := "normal"
			if fallback {
				path = "fallback"
			}
			total := make([][]float64, 2)
			hold := make([][]float64, 2)
			for repetition := -1; repetition < 3; repetition++ {
				for slot := 0; slot < 2; slot++ {
					index := (slot + (repetition+1)%2) % 2
					variant := variants[index]
					*counts = materialObservation{}
					start := time.Now()
					_, err := variant.read(t.Context(), observed, target)
					elapsed := time.Since(start)
					if err != nil {
						t.Fatalf("%s %s: %v", path, variant.name, err)
					}
					if counts.Commits != 1 || counts.Rollbacks != 0 || counts.Hold <= 0 || (counts.Fallback.Calls == 1) != fallback || fallback && counts.ProjectionState != "22P05" {
						t.Fatalf("invalid observation %+v", counts)
					}
					if !fallback && counts.Projection.Rows != 7687 {
						t.Fatalf("projection %+v", counts)
					}
					if repetition < 0 {
						continue
					}
					t.Logf("sample candidates=%d%% path=%s repetition=%d slot=%d variant=%s total_ms=%.3f hold_ms=%.3f projection=%+v selected=%+v fallback=%+v", percent, path, repetition+1, slot+1, variant.name, float64(elapsed)/1e6, float64(counts.Hold)/1e6, counts.Projection, counts.Selected, counts.Fallback)
					total[index] = append(total[index], float64(elapsed)/1e6)
					hold[index] = append(hold[index], float64(counts.Hold)/1e6)
				}
			}
			for i, variant := range variants {
				sort.Float64s(total[i])
				sort.Float64s(hold[i])
				t.Logf("summary candidates=%d%% path=%s variant=%s total_ms[min/median/max]=%v hold_ms[min/median/max]=%v", percent, path, variant.name, total[i], hold[i])
			}
		}
	}
}

// Deterministic 128-rank profiles, linearly interpolated at ranks 0,19/44,
// 63,121,127. Rank (i*73)%128 repeats. All payloads are ASCII synthetic strings.
// These marginal profiles are not a reconstruction of the joint distribution.
func materialProfile(rank, empty, median, p95, max int) int {
	points := [][2]int{{0, 0}, {empty - 1, 0}, {63, median}, {121, p95}, {127, max}}
	for i := 1; i < len(points); i++ {
		if rank <= points[i][0] {
			a, b := points[i-1], points[i]
			return a[1] + (b[1]-a[1])*(rank-a[0])/(b[0]-a[0])
		}
	}
	return max
}
func seedMaterialMeasurement(t testing.TB, records *consultations.Records, percent int) Record {
	t.Helper()
	const n = 7687
	const capacity = 362545211
	ctx := t.Context()
	if _, err := records.DB.ExecContext(ctx, "DELETE FROM drafts"); err != nil {
		t.Fatal(err)
	}
	target := Record{"id": "m00000", "project": "owner/repo", "status": "submitting", "createdAt": "2026-11-07T00:00:00Z", "parentConsultation": "m00001", "body": "supplied target"}
	// Scale only synthetic saved-JSON padding to the observed total capacity.
	weights := make([]int, n)
	sum := 0
	for i := range weights {
		rank := (i * 73) % 128
		weights[i] = 18000 + materialProfile(rank, 2, 44753-18000, 111355-18000, 385481-18000)
		sum += weights[i]
	}
	tx, err := records.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO drafts(id,json) VALUES($1,$2)")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	saved, bodySizes, messageSizes, messageCounts := []int{}, []int{}, []int{}, []int{}
	candidates := 0
	storedTotal := 0
	bodyTotal := 0
	messagesTotal := 0
	messageTotalCount := 0
	cumulative := 0
	previous := 0
	for i := 0; i < n; i++ {
		rank := (i * 73) % 128
		bodySize := materialProfile(rank, 20, 2693, 13425, 44066)
		count := materialProfile(rank, 45, 2, 7, 14)
		// Integer interpolation would create >45 empty message arrays. Keep positive
		// ranks positive; report the actual count distribution rather than hiding it.
		if rank >= 45 && count == 0 {
			count = 1
		}
		budget := materialProfile(rank, 45, 5083, 40510, 79381)
		messages := []any{}
		if count > 0 {
			for j := 0; j < count; j++ {
				messages = append(messages, Record{"role": "user", "text": ""})
			}
			overhead, _ := json.Marshal(messages)
			remaining := budget - len(overhead)
			if remaining < 0 {
				remaining = 0
			}
			for j := 0; j < count; j++ {
				size := remaining / (count - j)
				obj(messages[j])["text"] = strings.Repeat("m", size)
				remaining -= size
			}
		}
		id := fmt.Sprintf("m%05d", i)
		row := Record{"id": id, "project": "other/repo", "status": "closed", "createdAt": "2026-11-01T00:00:00Z", "title": "ordinary", "body": strings.Repeat("b", bodySize), "messages": messages, "syntheticPadding": ""}
		if i == 0 {
			for k, v := range target {
				if k != "body" {
					row[k] = v
				}
			}
		}
		if i >= 1 && i <= 256 {
			if i < 256 {
				row["parentConsultation"] = fmt.Sprintf("m%05d", i+1)
			}
		}
		// Candidate share refers to the 7429 non-target/non-ancestor rows. Deterministic
		// evenly spaced eligibility; remaining rows are other-project and inactive.
		if i > 256 && ((i-257)*percent/100 != (i-256)*percent/100) {
			row["project"] = "owner/repo"
			row["status"] = "draft"
			candidates++
		}
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		cumulative += weights[i]
		desired := int(int64(capacity)*int64(cumulative)/int64(sum)) - previous
		previous += desired
		if desired < len(raw) {
			t.Fatalf("profile cannot contain payload: row=%d desired=%d actual=%d", i, desired, len(raw))
		}
		row["syntheticPadding"] = strings.Repeat("p", desired-len(raw))
		raw, err = json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = stmt.ExecContext(ctx, "db-"+id, string(raw)); err != nil {
			t.Fatal(err)
		}
		msg, _ := json.Marshal(messages)
		saved = append(saved, len(raw))
		bodySizes = append(bodySizes, bodySize)
		messageSizes = append(messageSizes, len(msg))
		messageCounts = append(messageCounts, count)
		storedTotal += len(raw)
		bodyTotal += bodySize
		messagesTotal += len(msg)
		messageTotalCount += count
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if storedTotal != capacity {
		t.Fatalf("saved bytes=%d", storedTotal)
	}
	t.Logf("fixture candidate_target=%d%% eligible=%d/7429 actual=%.4f%% ancestors=256, parentConsultation chain m00000..m00256; candidates project=owner/repo status=draft; other rows project=other/repo status=closed; target created=2026-11-07T00:00:00Z others=2026-11-01T00:00:00Z", percent, candidates, 100*float64(candidates)/7429)
	for _, metric := range []struct {
		name                                string
		values                              []int
		total, observedTotal, p50, p95, max int
	}{
		{"saved-json", saved, storedTotal, 362545211, 44753, 111355, 385481},
		{"body", bodySizes, bodyTotal, 541190, 2693, 13425, 44066},
		{"messages-json", messageSizes, messagesTotal, 1559525, 5083, 40510, 79381},
		{"messages-count", messageCounts, messageTotalCount, 270, 2, 7, 14},
	} {
		sort.Ints(metric.values)
		q := func(p float64) int { return metric.values[int(math.Ceil(float64(n)*p))-1] }
		// Only saved-json totals have the same population as the observation. The
		// others compare a generated per-128 mean, explicitly not a full-DB estimate.
		mean128 := float64(metric.total) * 128 / n
		if metric.name == "saved-json" {
			mean128 = float64(metric.total)
		}
		t.Logf("distribution %s total=%d p50/p95/max=%d/%d/%d deltas=%d/%d/%d comparable_total_or_per128=%.2f observed=%d delta=%.2f", metric.name, metric.total, q(.5), q(.95), q(1), q(.5)-metric.p50, q(.95)-metric.p95, q(1)-metric.max, mean128, metric.observedTotal, mean128-float64(metric.observedTotal))
	}
	return target
}
