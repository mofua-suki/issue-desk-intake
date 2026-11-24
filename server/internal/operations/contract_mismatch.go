package operations

import (
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type mismatch struct {
	Count       int
	First, Last time.Time
	Updating    bool
}
type ContractMismatches struct {
	mu   sync.Mutex
	rows map[string]*mismatch
}

var contractHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ContractUpdating(runtime string, now time.Time) bool {
	if runtime == "" {
		return false
	}
	raw, e := os.ReadFile(filepath.Join(runtime, "state", "desk-contract-update.until"))
	if e != nil || len(raw) > 100 {
		return false
	}
	until, e := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	return e == nil && until == float64(int64(until)) && until > float64(now.UnixMilli()) && until <= float64(now.UnixMilli()+900000)
}
func (m *ContractMismatches) Record(incoming string, updating bool, now time.Time) {
	if !contractHash.MatchString(incoming) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]*mismatch{}
	}
	r := m.rows[incoming]
	if r == nil {
		if len(m.rows) >= 8 {
			oldest := ""
			for version, value := range m.rows {
				if oldest == "" || value.Last.Before(m.rows[oldest].Last) {
					oldest = version
				}
			}
			delete(m.rows, oldest)
		}
		r = &mismatch{First: now, Last: now, Updating: updating}
		m.rows[incoming] = r
	}
	if updating {
		r.Updating = true
		return
	}
	if r.Count == 0 {
		r.First = now
	}
	r.Count++
	r.Last = now
}
func (m *ContractMismatches) Alerts(runtime string, now time.Time) []any {
	out := []any{}
	if m == nil || ContractUpdating(runtime, now) {
		return out
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	versions := []string{}
	for v, r := range m.rows {
		if r.Count >= 2 && !r.Last.After(now) && now.Sub(r.Last) <= 30*time.Minute {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return out
	}
	sort.Strings(versions)
	detail := []string{"受付の版: " + contract.Version}
	for _, v := range versions {
		r := m.rows[v]
		detail = append(detail, fmt.Sprintf("送られた版: %s / %d回 / 最初 %s / 最後 %s", v, r.Count, iso(r.First), iso(r.Last)))
	}
	detail = append(detail, "確かめる場所: NASの受付の更新、PCのmainの更新、hostdの配布（docs/desk-contract.md）。", "最後の不一致から30分たつか、受付が起動し直すと、この要対応は消えます。")
	return append(out, map[string]any{"id": "desk-contract-mismatch", "kind": "desk_contract_mismatch", "status": "stall_alert", "project": fleet.SystemProject, "title": "依頼側の scripts と受付の取り決めの版がずれています", "detail": strings.Join(detail, "\n")})
}
