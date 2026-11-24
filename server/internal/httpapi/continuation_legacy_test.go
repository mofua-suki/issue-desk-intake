package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/jobs"
)

func TestWorkerLegacyArtifactStateContract(t *testing.T) {
	for _, tc := range []struct {
		name, historyHost string
		want              int
	}{
		{"same PC", "nas", http.StatusOK},
		// Report could never prove these owners, so dispatch restarts from the base.
		{"different previous PC", "other", http.StatusOK},
		{"no previous history", "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, db, token := workerFixture(t)
			status, _ := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":2}`)
			if status != http.StatusOK {
				t.Fatal(status)
			}
			var id int64
			err := db.QueryRow(`INSERT INTO jobs(repo,issue,status,lease_epoch,branch,head_sha) VALUES('owner/repo',1,'queued',1,'old-branch',$1) RETURNING id`, strings.Repeat("a", 40)).Scan(&id)
			if err != nil {
				t.Fatal(err)
			}
			if tc.historyHost != "" {
				if _, err := db.Exec(`INSERT INTO job_events(job_id,host,kind,data) VALUES($1,$2,'leased','{"epoch":1,"generation":0}')`, id, tc.historyHost); err != nil {
					t.Fatal(err)
				}
			}
			status, taken := workerRequest(t, handler, token, "POST", "/api/v1/jobs/next", `{"capacity":2}`)
			if status != http.StatusOK {
				t.Fatalf("dispatch: %d %v", status, taken)
			}
			base := "/api/v1/jobs/" + fmtInt(id)
			// Workers cannot manufacture the leased evidence used for compatibility.
			status, _ = workerRequest(t, handler, token, "POST", base+"/events", `{"epoch":2,"kind":"leased","data":{"epoch":1}}`)
			if status != http.StatusBadRequest {
				t.Fatalf("forged lease event: %d", status)
			}
			body := `{"epoch":2,"status":"failed","branch":"new-branch","headSha":"` + strings.Repeat("b", 40) + `","view":{"continuation":{"host":"nas","artifact":{"owner":"nas"},"publication":{"verified":true}}}}`
			status, reported := workerRequest(t, handler, token, "POST", base+"/state", body)
			if status != tc.want {
				t.Fatalf("report: %d %v", status, reported)
			}
			if tc.want == http.StatusConflict && reported["code"] != "artifact_unavailable" {
				t.Fatalf("wrong error: %v", reported)
			}
			status, detail := workerRequest(t, handler, token, "GET", base, "")
			if status != http.StatusOK {
				t.Fatal(status, detail)
			}
			job := detail["job"].(map[string]any)
			if reset, _ := job["view"].(map[string]any)["retryFromBase"].(map[string]any); (reset["by"] == "dispatch:artifact_unknown") != (tc.historyHost != "nas") {
				t.Fatalf("dispatch reset: %v", job["view"])
			}
			if tc.want == http.StatusOK {
				c := job["view"].(map[string]any)["continuation"].(map[string]any)
				artifact := c["artifact"].(map[string]any)
				if job["status"] != "failed" || job["leaseExpiresAt"] != nil || artifact["owner"] != "nas" || artifact["headSha"] != strings.Repeat("b", 40) || artifact["branch"] != "new-branch" || artifact["generation"] != job["generation"] || c["publication"] != nil {
					t.Fatalf("persisted API result: %v", job)
				}
				status, _ = workerRequest(t, handler, token, "POST", base+"/state", body)
				if status != http.StatusOK {
					t.Fatalf("resend: %d", status)
				}
			} else {
				view := job["view"].(map[string]any)
				if job["headSha"] != strings.Repeat("a", 40) || job["branch"] != "old-branch" || job["status"] != "running" || view["continuation"] != nil {
					raw, _ := json.Marshal(job)
					t.Fatalf("rejected report changed saved API result: %s", raw)
				}
			}
		})
	}
}

func TestWorkerRetryModesKeepInvalidArtifact(t *testing.T) {
	for _, mode := range []string{"normal", "retest", "autoRetest", "fromBase"} {
		t.Run(mode, func(t *testing.T) {
			handler, db, token := workerFixture(t)
			status, _ := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":2}`)
			if status != http.StatusOK {
				t.Fatal(status)
			}
			var id int64
			err := db.QueryRow(`INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,branch,head_sha,view,result)
VALUES('owner/repo',1,'failed','nas',1,'old-branch',$1,'{"continuation":{"artifact":null}}',
'{"status":"failed","testFailures":{"complete":true,"names":["fixed regression"]}}') RETURNING id`, strings.Repeat("a", 40)).Scan(&id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO job_events(job_id,host,kind,data) VALUES($1,'nas','leased','{"epoch":1,"generation":0}')`, id); err != nil {
				t.Fatal(err)
			}
			s, err := jobs.New(db, jobs.Options{})
			if err != nil {
				t.Fatal(err)
			}
			// Control is setup through the same intake transaction used by retry.
			_, err = s.Control(t.Context(), id, jobs.ControlInput{ID: "22222222-2222-4222-8222-222222222222", Action: "retry", By: "issue-screen",
				Retest: mode == "retest", AutoRetest: mode == "autoRetest", FromBase: mode == "fromBase"}, jobs.ControlOptions{})
			if err != nil {
				t.Fatal(err)
			}
			status, taken := workerRequest(t, handler, token, "POST", "/api/v1/jobs/next", `{"capacity":2}`)
			if status != http.StatusOK || taken["job"] == nil {
				t.Fatalf("dispatch: %d %v", status, taken)
			}
			base := "/api/v1/jobs/" + fmtInt(id)
			status, before := workerRequest(t, handler, token, "GET", base, "")
			if status != http.StatusOK {
				t.Fatal(status, before)
			}
			body := `{"epoch":2,"status":"failed","branch":"new-branch","headSha":"` + strings.Repeat("b", 40) + `","view":{"continuation":{"artifact":{"owner":"nas"}}}}`
			status, reported := workerRequest(t, handler, token, "POST", base+"/state", body)
			// No PC can report a head change for the invalid record, so dispatch
			// restarts the other modes from the base instead of letting the lease expire.
			if status != http.StatusOK {
				t.Fatalf("reset report: %d %v", status, reported)
			}
			status, after := workerRequest(t, handler, token, "GET", base, "")
			if status != http.StatusOK {
				t.Fatal(status, after)
			}
			job := after["job"].(map[string]any)
			want := "issue-screen"
			if mode != "fromBase" {
				want = "dispatch:artifact_unknown"
			}
			if reset, _ := job["view"].(map[string]any)["retryFromBase"].(map[string]any); reset["by"] != want {
				t.Fatalf("reset by: %v want %s", job["view"], want)
			}
			artifact := job["view"].(map[string]any)["continuation"].(map[string]any)["artifact"].(map[string]any)
			if job["status"] != "failed" || job["leaseExpiresAt"] != nil || job["branch"] != "new-branch" || artifact["owner"] != "nas" || artifact["headSha"] != strings.Repeat("b", 40) || artifact["generation"] != job["generation"] {
				t.Fatalf("reset did not save new artifact: %v", job)
			}
		})
	}
}
