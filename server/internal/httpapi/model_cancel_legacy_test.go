package httpapi

import (
	"issue-desk-intake/server/internal/auth"
	"strings"
	"testing"
	"time"
)

// Legacy entries have the same JSON and settings columns used before model
// selection became fixed. The route must delete only the requested step.
func TestCancelLegacyModelOverridesHTTP(t *testing.T) {
	ungranted := "idk_" + strings.Repeat("b", 43)
	handler, db, token := workerFixtureOptions(t, true, func(o *WorkerRouteOptions) {
		if _, err := o.AppDB.Exec(`INSERT INTO api_tokens(id,name,hash,issued_at) VALUES(?,?,?,?)`, "no-grant", "pc", auth.Hash(ungranted), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	})
	legacy := `{"provider":"claude","model":"legacy-model","effort":"high","setAt":"2026-01-01T00:00:00Z","until":"2026-01-02T00:00:00Z"}`
	steps := []string{"intake", "review", "priority"}
	for _, step := range steps {
		if _, err := db.Exec(`INSERT INTO settings(key,value,expires_at,updated_by) VALUES($1,$2,'2026-01-02T00:00:00Z','fixture')`, "modelOverride."+step, legacy); err != nil {
			t.Fatal(err)
		}
	}
	for i, step := range steps {
		status, view := workerRequest(t, handler, token, "GET", "/api/model-overrides", "")
		if status != 200 || len(view["overrides"].([]any)) != 0 {
			t.Fatalf("fixed view before %s: %d %v", step, status, view)
		}
		path := "/api/model-overrides/" + step + "/cancel"
		status, denied := workerRequest(t, handler, ungranted, "POST", path, `{}`)
		if status != 403 {
			t.Fatalf("ungranted cancel %s: %d %v", step, status, denied)
		}
		status, result := workerRequest(t, handler, token, "POST", path, `{}`)
		if status != 200 || result["cancelled"] != true {
			t.Fatalf("cancel %s: %d %v", step, status, result)
		}
		for j, other := range steps {
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM settings WHERE key=$1`, "modelOverride."+other).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (count == 1) != (j > i) {
				t.Fatalf("after cancelling %s, %s count=%d", step, other, count)
			}
		}
		status, result = workerRequest(t, handler, token, "POST", path, `{}`)
		if status != 200 || result["cancelled"] != false {
			t.Fatalf("repeat %s: %d %v", step, status, result)
		}
	}
}
