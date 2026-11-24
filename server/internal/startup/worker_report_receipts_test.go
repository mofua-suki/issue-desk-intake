package startup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/httpapi"
	"issue-desk-intake/server/internal/operations"
	"issue-desk-intake/server/internal/store"
	"issue-desk-intake/server/internal/workplan"
)

func TestWorkerReportReceiptsWiring(t *testing.T) {
	app := reviewNotesApp(t)
	dir := t.TempDir()
	fleetPath, assignmentsPath := filepath.Join(dir, "fleet.json"), filepath.Join(dir, "assignments.json")
	writeConfig(t, fleetPath, map[string]any{"version": 1, "defaults": map[string]any{}, "hosts": map[string]any{"nas": map[string]any{"giteaUrl": "http://gitea.example", "repositories": []any{map[string]any{"name": "owner/repo"}}}}})
	writeConfig(t, assignmentsPath, map[string]any{"pollSeconds": 300, "hosts": map[string]any{"nas": map[string]any{"maxImplementations": 1, "repositories": map[string]any{"owner/repo": map[string]any{"rank": 1}}}}, "repositories": map[string]any{}, "workerAccounts": map[string]any{"nas": "worker-nas"}})
	app.Config.FleetPath, app.Config.AssignmentsPath = fleetPath, assignmentsPath
	app.Fleet = &fleet.Service{DB: app.DB, FleetPath: fleetPath, AssignmentsPath: assignmentsPath}
	app.AIFailures = &operations.AIFailures{}
	t.Cleanup(func() { app.AIFailures = nil })
	var commentBody, author string
	git := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/repos/owner/repo/issues/") || !strings.HasSuffix(r.URL.Path, "/comments") {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode([]any{map[string]any{"id": 7, "body": commentBody, "user": map[string]any{"login": author}}})
	}))
	defer git.Close()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	app.Gitea, err = gitea.New(git.URL, tokenFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := "idk_" + strings.Repeat("a", 43)
	if _, err = app.SQLite.AddPCToken(t.Context(), store.PCTokenRecord{ID: "worker", Name: "nas", Hash: auth.Hash(token), IssuedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	app.Auth, err = auth.New(auth.Config{Port: 8787}, &store.AuthStore{SQLite: app.SQLite, Postgres: app.DB})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err = httpapi.RegisterWorkerRoutes(mux, app.workerOptions()); err != nil {
		t.Fatal(err)
	}
	plan := &workplan.Plan{Version: 1, Requirements: []workplan.Requirement{{ID: "one", Criterion: "報告を確認", Owner: "reporter", Phase: "report", Method: "投稿を照合", Expected: "一致", EvidenceKind: "report_comment", Capabilities: []string{}, Inputs: []string{}, Children: []int{}}}}
	head := strings.Repeat("a", 40)
	for i, tc := range []struct {
		name                      string
		badAuthor, badHash, stale bool
	}{{"success", false, false, false}, {"author", true, false, false}, {"hash", false, true, false}, {"epoch", false, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			view, _ := json.Marshal(map[string]any{"executionPlan": plan.Record()})
			var id int64
			err := app.DB.QueryRowContext(t.Context(), `INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,head_sha,view) VALUES('owner/repo',$4,'running','nas',7,$1,$2,$3::jsonb) RETURNING id`, time.Now().Add(time.Hour), head, view, i+1).Scan(&id)
			if err != nil {
				t.Fatal(err)
			}
			marker := fmt.Sprintf("<!-- issue-desk-execution-report:%d:%s:%s -->", id, head, plan.Fingerprint())
			commentBody = "報告と根拠\n" + marker
			author = "worker-nas"
			if tc.badAuthor {
				author = "other"
			}
			sum := sha256.Sum256([]byte(commentBody))
			hash := hex.EncodeToString(sum[:])
			if tc.badHash {
				hash = strings.Repeat("0", 64)
			}
			epoch := 7
			if tc.stale {
				epoch = 6
			}
			body, _ := json.Marshal(map[string]any{"host": "nas", "epoch": epoch, "status": "merged", "controllerReport": map[string]any{"planSHA": plan.Fingerprint(), "head": head, "marker": marker, "bodySHA": hash}})
			send := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", fmt.Sprintf("http://localhost:8787/api/v1/jobs/%d/state", id), bytes.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set(contract.Header, contract.Version)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				return w
			}
			response := send()
			want := 200
			if tc.badAuthor || tc.badHash || tc.stale {
				want = 409
			}
			if response.Code != want {
				t.Fatalf("HTTP %d wanted %d: %s", response.Code, want, response.Body.String())
			}
			var status string
			var saved []byte
			if err := app.DB.QueryRowContext(t.Context(), `SELECT status,view FROM jobs WHERE id=$1`, id).Scan(&status, &saved); err != nil {
				t.Fatal(err)
			}
			var stored map[string]any
			if err := json.Unmarshal(saved, &stored); err != nil {
				t.Fatal(err)
			}
			receipts, err := workplan.DecodeEvidence(stored["executionReceipts"])
			if err != nil {
				t.Fatal(err)
			}
			if want == 409 {
				if len(receipts) != 0 || status != "running" {
					t.Fatal(status, receipts)
				}
				return
			}
			if status != "merged" || len(receipts) != 1 || receipts[0].RecordID != "comment:7" || receipts[0].Source != "reporter" {
				t.Fatal(status, receipts)
			}
			response = send()
			if response.Code != 200 {
				t.Fatalf("replay HTTP %d: %s", response.Code, response.Body.String())
			}
			var count int
			if err := app.DB.QueryRowContext(t.Context(), `SELECT count(*) FROM job_events WHERE job_id=$1 AND kind='state' AND data->>'to'='merged'`, id).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
			if err := app.DB.QueryRowContext(t.Context(), `SELECT view FROM jobs WHERE id=$1`, id).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(saved, &stored); err != nil {
				t.Fatal(err)
			}
			receipts, err = workplan.DecodeEvidence(stored["executionReceipts"])
			if err != nil || len(receipts) != 1 || receipts[0].RecordID != "comment:7" {
				t.Fatal(receipts, err)
			}
		})
	}
}
