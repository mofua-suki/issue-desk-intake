package operations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRepositoryReadsWarnAndRaiseAttention(t *testing.T) {
	at := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	logs := []string{}
	reads := &RepositoryReads{}
	failing := map[string]bool{"owner/gone": true}
	read := func(_ context.Context, repo string) error {
		if failing[repo] {
			return errors.New("not found")
		}
		return nil
	}
	reason := func(error) string { return "GiteaがHTTP 404を返しました。" }
	if err := reads.Check(context.Background(), []string{"owner/repo", "owner/gone"}, read, reason, func() time.Time { return at }, func(s string) { logs = append(logs, s) }); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "警告") || !strings.Contains(logs[0], "owner/gone") || !strings.Contains(logs[0], "404") {
		t.Fatalf("warning: %v", logs)
	}
	alerts, err := reads.Alerts(context.Background())
	if err != nil || len(alerts) != 1 {
		t.Fatalf("alerts: %v %v", alerts, err)
	}
	row := alerts[0].(map[string]any)
	if row["kind"] != "intake_repository_unreadable" || row["status"] != "stall_alert" || row["repo"] != "owner/gone" || row["detectedAt"] != iso(at) || !strings.Contains(row["title"].(string), "owner/gone") || !strings.Contains(row["detail"].(string), "404") {
		t.Fatalf("alert: %v", row)
	}
	// A cancelled check keeps the previous result.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reads.Check(cancelled, []string{"owner/gone"}, read, reason, time.Now, nil); err == nil {
		t.Fatal("cancelled check succeeded")
	}
	if alerts, _ := reads.Alerts(context.Background()); len(alerts) != 1 {
		t.Fatalf("cancelled check replaced the result: %v", alerts)
	}
	// Once readable, the attention item disappears and the recovery is logged.
	delete(failing, "owner/gone")
	logs = nil
	if err := reads.Check(context.Background(), []string{"owner/repo", "owner/gone"}, read, reason, time.Now, func(s string) { logs = append(logs, s) }); err != nil {
		t.Fatal(err)
	}
	if alerts, _ := reads.Alerts(context.Background()); len(alerts) != 0 || len(logs) != 1 || !strings.Contains(logs[0], "読めるようになりました") {
		t.Fatalf("recovered: %v %v", alerts, logs)
	}
}

// Periodic rechecks follow the current candidates: a persistent failure keeps
// its first detection time and is not logged again, a removed candidate's
// record disappears, and a recovery is logged once.
func TestRepositoryReadsFollowCurrentCandidates(t *testing.T) {
	first := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	at := first
	logs := []string{}
	reads := &RepositoryReads{}
	failing := map[string]bool{"owner/late": true, "owner/removed": true}
	read := func(_ context.Context, repo string) error {
		if failing[repo] {
			return errors.New("connection refused")
		}
		return nil
	}
	check := func(repos ...string) []any {
		t.Helper()
		if err := reads.Check(context.Background(), repos, read, func(e error) string { return e.Error() }, func() time.Time { return at }, func(s string) { logs = append(logs, s) }); err != nil {
			t.Fatal(err)
		}
		alerts, err := reads.Alerts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}
	if alerts := check("owner/late", "owner/removed"); len(alerts) != 2 || len(logs) != 2 {
		t.Fatalf("first: %v %v", alerts, logs)
	}
	// Still failing: one record per candidate, first detection kept, no repeated warning.
	at = first.Add(2 * time.Minute)
	logs = nil
	alerts := check("owner/late", "owner/removed")
	if len(alerts) != 2 || len(logs) != 0 || alerts[0].(map[string]any)["detectedAt"] != iso(first) {
		t.Fatalf("recheck: %v %v", alerts, logs)
	}
	// The removed candidate is no longer checked and its record disappears.
	alerts = check("owner/late")
	if len(alerts) != 1 || alerts[0].(map[string]any)["repo"] != "owner/late" || len(logs) != 0 {
		t.Fatalf("removed: %v %v", alerts, logs)
	}
	// Gitea came up late: the next check clears the item and logs the recovery.
	delete(failing, "owner/late")
	if alerts = check("owner/late", "owner/added"); len(alerts) != 0 || len(logs) != 1 || !strings.Contains(logs[0], "owner/late") {
		t.Fatalf("recovered: %v %v", alerts, logs)
	}
}
