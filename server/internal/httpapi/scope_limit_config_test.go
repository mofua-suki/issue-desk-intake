package httpapi

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestWorkerScopeLimitConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, split string
		limit       int
	}{
		{"missing", "", 8}, {"missing_files", `{}`, 8},
		{"null", `{"files":null}`, 8}, {"zero", `{"files":0}`, 8},
		{"negative", `{"files":-1}`, 8}, {"fraction", `{"files":1.5}`, 8},
		{"string", `{"files":"12"}`, 8}, {"boolean", `{"files":true}`, 8},
		{"unsafe", `{"files":9007199254740992}`, 8},
		{"explicit_six", `{"files":6}`, 6}, {"explicit_twelve", `{"files":12}`, 12},
		{"shipping", "", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, db, token := workerFixtureOptions(t, false, func(options *WorkerRouteOptions) {
				data, err := os.ReadFile(options.AssignmentsPath)
				if tc.name == "shipping" {
					data, err = os.ReadFile("../../../assignments.json")
				}
				if err != nil {
					t.Fatal(err)
				}
				var raw map[string]any
				if err := json.Unmarshal(data, &raw); err != nil {
					t.Fatal(err)
				}
				// The fixture fleet has no Mac entries, which test-only hosts require.
				hosts, _ := raw["hosts"].(map[string]any)
				accounts, _ := raw["workerAccounts"].(map[string]any)
				for host, entry := range hosts {
					if value, _ := entry.(map[string]any); value["role"] == "test-only" {
						delete(hosts, host)
						delete(accounts, host)
					}
				}
				if tc.split != "" {
					var split any
					if err := json.Unmarshal([]byte(tc.split), &split); err != nil {
						t.Fatal(err)
					}
					raw["intakeSplit"] = split
				}
				data, err = json.Marshal(raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(options.AssignmentsPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			})
			var id int64
			if err := db.QueryRow(`INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,scope,scope_source) VALUES('owner/repo',1,'running','nas',1,now()+interval '1 hour',ARRAY['a','b'],'declared') RETURNING id`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			path := "/api/v1/jobs/" + fmtInt(id) + "/scope"
			for total := 3; total <= tc.limit+1; total++ {
				status, value := workerRequest(t, handler, token, "POST", path, fmt.Sprintf(`{"epoch":1,"add":["file%d"]}`, total))
				if total <= tc.limit {
					if status != 200 || value["result"] != "granted" {
						t.Fatalf("total %d: %d %v", total, status, value)
					}
				} else if status != 409 || value["code"] != "lease_lost" || value["result"] != nil {
					t.Fatalf("over: %d %v", status, value)
				}
			}
			var status string
			var length, savedLimit int
			if err := db.QueryRow(`SELECT status,cardinality(scope),(SELECT (data->>'limit')::int FROM job_events WHERE job_id=$1 AND kind='scope_over_limit') FROM jobs WHERE id=$1`, id).Scan(&status, &length, &savedLimit); err != nil || status != "stopped" || length != tc.limit || savedLimit != tc.limit {
				t.Fatalf("stored: %s %d %d %v", status, length, savedLimit, err)
			}
		})
	}
}

func TestWorkerScopeLimitRejectsBrokenAssignments(t *testing.T) {
	for _, body := range []string{"missing_file", `{`, `null`, `{"pollSeconds":0}`, `{"pollSeconds":300,"hosts":{},"intakeSplit":null}`} {
		t.Run(body, func(t *testing.T) {
			var assignmentPath string
			handler, db, token := workerFixtureOptions(t, false, func(options *WorkerRouteOptions) { assignmentPath = options.AssignmentsPath })
			var err error
			if body == "missing_file" {
				err = os.Remove(assignmentPath)
			} else {
				err = os.WriteFile(assignmentPath, []byte(body), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			var id int64
			if err := db.QueryRow(`INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,scope,scope_source) VALUES('owner/repo',1,'running','nas',1,now()+interval '1 hour',ARRAY['a'],'declared') RETURNING id`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			status, value := workerRequest(t, handler, token, "POST", "/api/v1/jobs/"+fmtInt(id)+"/scope", `{"epoch":1,"add":["b"]}`)
			if status < 400 || value["code"] == "lease_lost" {
				t.Fatalf("broken assignments accepted: %d %v", status, value)
			}
			var intact bool
			if err := db.QueryRow(`SELECT status='running' AND scope=ARRAY['a'] AND NOT EXISTS(SELECT 1 FROM job_events WHERE job_id=$1) FROM jobs WHERE id=$1`, id).Scan(&intact); err != nil || !intact {
				t.Fatalf("state changed: %v %v", intact, err)
			}
		})
	}
}

func TestWorkerScopeLimitPreservesOverlapWait(t *testing.T) {
	handler, db, token := workerFixture(t)
	if _, err := db.Exec(`INSERT INTO jobs(id,repo,issue,status,lease_host,lease_epoch,lease_expires_at,scope,scope_source) VALUES(1,'owner/repo',1,'running','nas',1,now()+interval '1 hour',ARRAY['a'],'declared'),(2,'owner/repo',2,'running','other',1,now()+interval '1 hour',ARRAY['shared.go','shared_test.go'],'declared')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO merge_turns(repo,holder_host,job_id,expires_at) VALUES('owner/repo','nas',1,now()+interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	status, value := workerRequest(t, handler, token, "POST", "/api/v1/jobs/1/scope", `{"epoch":1,"add":["shared_test.go"]}`)
	if status != 200 || value["result"] != "granted" {
		t.Fatalf("test overlap: %d %v", status, value)
	}
	status, value = workerRequest(t, handler, token, "POST", "/api/v1/jobs/1/scope", `{"epoch":1,"add":["shared.go"]}`)
	if status != 200 || value["result"] != "wait" {
		t.Fatalf("production overlap: %d %v", status, value)
	}
	var held bool
	if err := db.QueryRow(`SELECT status='held' AND hold_reason='scope_wait' AND lease_expires_at IS NULL AND 'shared.go'=ANY(scope) AND EXISTS(SELECT 1 FROM merge_turns WHERE repo='owner/repo' AND job_id IS NULL) AND EXISTS(SELECT 1 FROM job_events WHERE job_id=1 AND kind='scope_wait') FROM jobs WHERE id=1`).Scan(&held); err != nil || !held {
		t.Fatalf("wait state: %v %v", held, err)
	}
}

func TestWorkerScopeLimitPersistenceFailures(t *testing.T) {
	for _, phase := range []string{"IMMEDIATE", "DEFERRED"} {
		t.Run(phase, func(t *testing.T) {
			handler, db, token := workerFixture(t)
			if _, err := db.Exec(`INSERT INTO jobs(id,repo,issue,status,lease_host,lease_epoch,lease_expires_at,scope,scope_source) VALUES(1,'owner/repo',1,'running','nas',1,now()+interval '1 hour',ARRAY['a'],'declared')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE FUNCTION reject_scope_stop() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='scope_over_limit' THEN RAISE EXCEPTION 'injected stop failure'; END IF; RETURN NEW; END $$;
CREATE CONSTRAINT TRIGGER reject_scope_stop AFTER INSERT ON job_events DEFERRABLE INITIALLY ` + phase + ` FOR EACH ROW EXECUTE FUNCTION reject_scope_stop()`); err != nil {
				t.Fatal(err)
			}
			status, value := workerRequest(t, handler, token, "POST", "/api/v1/jobs/1/scope", `{"epoch":1,"add":["b","c","d","e","f","g","h","i"]}`)
			if status < 500 || value["code"] == "lease_lost" || value["result"] != nil {
				t.Fatalf("persistence failure: %d %v", status, value)
			}
			var intact bool
			if err := db.QueryRow(`SELECT status='running' AND scope=ARRAY['a'] AND lease_expires_at IS NOT NULL AND result IS NULL AND NOT EXISTS(SELECT 1 FROM job_events WHERE job_id=1) FROM jobs WHERE id=1`).Scan(&intact); err != nil || !intact {
				t.Fatalf("rollback: %v %v", intact, err)
			}
		})
	}
}
