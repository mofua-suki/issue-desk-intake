package httpapi

import (
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

func TestStateDiffFingerprintContractHTTP(t *testing.T) {
	otherToken := "idk_" + strings.Repeat("b", 43)
	handler, db, token := workerFixtureOptions(t, false, func(options *WorkerRouteOptions) {
		_, err := options.AppDB.Exec("INSERT INTO api_tokens(id,name,hash,issued_at) VALUES(?,?,?,?)", "other-token", "another-pc", auth.Hash(otherToken), time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
	})
	status, _ := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":2}`)
	if status != 200 {
		t.Fatal(status)
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO jobs(repo,issue,status) VALUES('owner/repo',1,'queued') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	status, taken := workerRequest(t, handler, token, "POST", "/api/v1/jobs/next", `{"capacity":2}`)
	if status != 200 || taken["job"] == nil {
		t.Fatal(status, taken)
	}
	path := "/api/v1/jobs/" + fmtInt(id) + "/state"
	head, fp := strings.Repeat("a", 40), "sha256:"+strings.Repeat("b", 64)
	body := `{"epoch":1,"status":"running","branch":"work","pr":1,"headSha":"` + head + `","diffFingerprint":"` + fp + `"}`
	status, result := workerRequest(t, handler, token, "POST", path, body)
	if status != 200 {
		t.Fatal(status, result)
	}
	stored := result["job"].(map[string]any)["view"].(map[string]any)["diffFingerprint"].(map[string]any)
	if stored["value"] != fp || stored["headSha"] != head {
		t.Fatal(stored)
	}
	for _, input := range []string{`""`, `"sha256:ABC"`, `42`} {
		status, _ = workerRequest(t, handler, token, "POST", path, `{"epoch":1,"status":"running","headSha":"`+head+`","diffFingerprint":`+input+`}`)
		if status != 400 {
			t.Fatalf("invalid %s: %d", input, status)
		}
	}
	status, _ = workerRequest(t, handler, token, "POST", path, `{"epoch":1,"status":"running","diffFingerprint":"`+fp+`"}`)
	if status != 400 {
		t.Fatal("missing head", status)
	}
	otherHead, otherFP := strings.Repeat("c", 40), "sha256:"+strings.Repeat("d", 64)
	rejected := `{"epoch":2,"status":"running","headSha":"` + otherHead + `","diffFingerprint":"` + otherFP + `"}`
	status, _ = workerRequest(t, handler, token, "POST", path, rejected)
	if status != 409 {
		t.Fatal(status)
	}
	status, _ = workerRequest(t, handler, otherToken, "POST", path, strings.Replace(rejected, `"epoch":2`, `"epoch":1`, 1))
	if status != 409 {
		t.Fatal(status)
	}
	status, _ = workerRequest(t, handler, token, "POST", path, `{"host":"another-pc","epoch":1,"status":"running","headSha":"`+otherHead+`","diffFingerprint":"`+otherFP+`"}`)
	if status != 403 {
		t.Fatal(status)
	}
	if _, err := db.Exec(`UPDATE jobs SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	status, _ = workerRequest(t, handler, token, "POST", path, strings.Replace(rejected, `"epoch":2`, `"epoch":1`, 1))
	if status != 409 {
		t.Fatal(status)
	}
	var value, storedHead string
	if err := db.QueryRow(`SELECT view->'diffFingerprint'->>'value', head_sha FROM jobs WHERE id=$1`, id).Scan(&value, &storedHead); err != nil || value != fp || storedHead != head {
		t.Fatal(value, storedHead, err)
	}
}
