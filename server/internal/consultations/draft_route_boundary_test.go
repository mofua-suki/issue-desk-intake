package consultations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDraftRouteDatabaseKeysAndFamilyFailure(t *testing.T) {
	s, db, _ := fixture(t)
	ctx := context.Background()
	for i, id := range []string{`null`, `"duplicate"`, `"duplicate"`, `42`, ``} {
		field := ""
		if id != "" {
			field = `,"id":` + id
		}
		raw := fmt.Sprintf(`{"project":"owner/app","status":"draft","body":"row-%d","familyProgress":{"old":true}%s}`, i, field)
		if _, err := db.DB().ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), fmt.Sprintf("key-%d", i), raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB().ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), "parent", `{"id":"parent","project":"owner/app","status":"draft","splitChildren":["child"],"splitIssues":[{}],"familyProgress":{"old":true}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, bindSQL("INSERT INTO drafts(id,json) VALUES(?,?)"), "child", `{"id":"child","project":"owner/app","status":"submitted","parentSplit":"parent","issue":{"number":7}}`); err != nil {
		t.Fatal(err)
	}
	key := stopToken(t, db, true)
	mux := http.NewServeMux()
	s.Register(mux)
	for _, failure := range []bool{false, true} {
		s.options.FamilyJobs = func(context.Context, []FamilyIssue) ([]FamilyJob, error) {
			if failure {
				return nil, errors.New("unavailable")
			}
			return []FamilyJob{{Project: "owner/app", Number: 7, Status: "merged"}}, nil
		}
		seen := map[string]bool{}
		for page := 1; page <= 7; page++ {
			req := httptest.NewRequest("GET", fmt.Sprintf("http://localhost:8787/api/drafts?limit=1&page=%d", page), nil)
			req.RemoteAddr = "127.0.0.1:1234"
			req.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var out struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Items) != 1 {
				t.Fatal(err, w.Body.String())
			}
			row := out.Items[0]
			if body, ok := row["body"].(string); ok {
				seen[body] = true
				if _, ok := row["familyProgress"]; ok {
					t.Fatalf("stale progress: %v", row)
				}
			}
			if row["id"] == "parent" {
				progress, ok := row["familyProgress"].(map[string]any)
				if failure && ok {
					t.Fatal(progress)
				}
				if !failure && (!ok || progress["completed"] != float64(1)) {
					t.Fatal(row)
				}
			}
		}
		for i := 0; i < 5; i++ {
			if !seen[fmt.Sprintf("row-%d", i)] {
				t.Fatal(seen)
			}
		}
	}
}
