package issueapi

import (
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

func TestScopeLimitAppearsInCommonNumberListPostgres(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pg.ExecContext(f.ctx, `UPDATE jobs SET status='running',scope=ARRAY['a','b','c','d'],scope_source='declared',lease_host='nas',lease_epoch=1,lease_expires_at=$2 WHERE id=$1`, f.job, f.options.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 12)
	for i := range paths {
		paths[i] = fmt.Sprintf("src/%02d-%s.go", i, strings.Repeat("x", 180))
	}
	got, err := f.options.Jobs.Widen(f.ctx, f.job, "nas", 1, paths, nil)
	var problem *contract.Problem
	if got != nil || !errors.As(err, &problem) || problem.Status != 409 || problem.Code != "lease_lost" {
		t.Fatalf("widen: %+v %v", got, err)
	}
	code, value := f.call("GET", "/api/issues", "screen", nil)
	rows := fleet.Items(value)
	if code != 200 || len(rows) != 1 {
		t.Fatalf("list %d: %v", code, value)
	}
	row := fleet.ObjectOrEmpty(rows[0])
	message := fleet.Text(row["error"])
	if row["desk"] == nil || row["status"] != "stopped" || !strings.Contains(message, "16件、上限8件、起票時4件") || !strings.Contains(message, "理由を付けて再開") || !strings.Contains(message, "閉じて分けて") || !strings.Contains(message, "他2件") {
		t.Fatalf("list row: %v", row)
	}
	for _, path := range paths[:10] {
		if !strings.Contains(message, path) {
			t.Fatalf("missing path %q in %q", path, message)
		}
	}
	if strings.Contains(message, paths[10]) {
		t.Fatal("unexpected eleventh path")
	}
	code, detail := f.call("GET", "/api/issue?project=owner/repo&number=1", "screen", nil)
	if code != 200 || fleet.ObjectOrEmpty(detail)["status"] != "stopped" || fleet.ObjectOrEmpty(detail)["error"] != message {
		t.Fatalf("detail %d: %v", code, detail)
	}
}
