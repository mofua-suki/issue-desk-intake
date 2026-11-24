package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hostd"
	"issue-desk-intake/server/internal/hosts"
	"strings"
	"testing"
)

func TestHostImageResultPreservesExistingResults(t *testing.T) {
	handler, db, _ := workerFixture(t)
	token := "idh_" + strings.Repeat("b", 43)
	if _, err := db.Exec(`INSERT INTO host_image_tokens(host,hash) VALUES('nas',$1)`, auth.Hash(token)); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	if _, err := db.Exec(`INSERT INTO host_image_results(host,tag,flavor,digest,status) VALUES('nas','bbbbbbbbbbbb','',$1,'success')`, digest); err != nil {
		t.Fatal(err)
	}
	status, response := workerRequest(t, handler, token, "POST", "/api/v1/hosts/nas/image-result", `{"tag":"aaaaaaaaaaaa","flavor":"","digest":"`+digest+`","status":"success"}`)
	if status != 200 || response["status"] != "success" {
		t.Fatalf("legacy result: %d %v", status, response)
	}
	var old, current, history int
	err := db.QueryRow(`SELECT (SELECT count(*) FROM host_image_results WHERE tag='bbbbbbbbbbbb' AND status='success'), (SELECT count(*) FROM host_image_results WHERE tag='aaaaaaaaaaaa' AND status='success'), (SELECT count(*) FROM host_image_history WHERE tag='aaaaaaaaaaaa' AND status='success' AND stage IS NULL AND reason IS NULL AND detail IS NULL AND self_check_failed IS NULL AND from_tag IS NULL AND started_at IS NULL AND received_at IS NOT NULL)`).Scan(&old, &current, &history)
	if err != nil || old != 1 || current != 1 || history != 1 {
		t.Fatalf("old=%d current=%d history=%d: %v", old, current, history, err)
	}
}

func TestHostdImageHealthAndDeploymentOwnership(t *testing.T) {
	handler, db, pc := workerFixture(t)
	token := "idh_" + strings.Repeat("b", 43)
	if _, err := db.Exec("INSERT INTO host_image_tokens(host,hash)VALUES('nas',$1)", auth.Hash(token)); err != nil {
		t.Fatal(err)
	}
	status, result := workerRequest(t, handler, pc, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":1}`)
	if status != 200 {
		t.Fatalf("heartbeat %d %v", status, result)
	}
	status, result = workerRequest(t, handler, token, "POST", "/api/v1/hosts/nas/hostd-health", `{"cycle":{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","phase":"finish","result":"success","stage":"complete","exceptionType":null,"at":"2026-11-01T00:00:00Z"}}`)
	if status != 200 || result["saved"] != true {
		t.Fatalf("health %d %v", status, result)
	}
	status, result = workerRequest(t, handler, token, "GET", "/api/v1/hosts/nas/hostd-health", "")
	if status != 200 || result["hostdHealth"] == nil {
		t.Fatalf("health read %d %v", status, result)
	}
	status, _ = workerRequest(t, handler, token, "GET", "/api/v1/hosts/other/desired-image", "")
	if status != 403 {
		t.Fatalf("other host %d", status)
	}
	status, _ = workerRequest(t, handler, pc, "GET", "/api/v1/hosts/nas/desired-image", "")
	if status != 403 {
		t.Fatalf("PC crossed credential boundary %d", status)
	}
	revision := strings.Repeat("a", 40)
	digest := "sha256:" + strings.Repeat("b", 64)
	if _, err := db.Exec("INSERT INTO releases(host,flavor,registry,tag,digest,revision)VALUES('*','','registry.example','aaaaaaaaaaaa',$1,$2)", digest, revision); err != nil {
		t.Fatal(err)
	}
	if _, e := (&hostd.Service{DB: db}).Desired(context.Background(), "nas", "", ""); e != nil {
		t.Fatal(e)
	}
	status, result = workerRequest(t, handler, token, "GET", "/api/v1/hosts/nas/desired-image", "")
	if status != 200 || result["desired"] == nil {
		t.Fatalf("desired %d %v", status, result)
	}
	status, result = workerRequest(t, handler, token, "POST", "/api/v1/hosts/nas/image-result", `{"tag":"aaaaaaaaaaaa","flavor":"","digest":"`+digest+`","status":"failed"}`)
	if status != 200 {
		t.Fatalf("failed result %d %v", status, result)
	}
	status, _ = workerRequest(t, handler, token, "POST", "/api/v1/hosts/nas/image-result", `{"tag":"aaaaaaaaaaaa","flavor":"","digest":"`+digest+`","status":"success"}`)
	if status != 200 {
		t.Fatal(status)
	}
	var saved string
	if err := db.QueryRow("SELECT status FROM host_image_results WHERE host='nas'").Scan(&saved); err != nil || saved != "failed" {
		t.Fatalf("failure overwritten %s %v", saved, err)
	}
	if _, err := db.Exec("INSERT INTO app_deployments(repo,commit_sha,deploy_host,status)VALUES('owner/repo',$1,'nas','running')", revision); err != nil {
		t.Fatal(err)
	}
	status, result = workerRequest(t, handler, token, "GET", "/api/v1/hosts/nas/deploy-next", "")
	if status != 200 || result["job"] == nil {
		t.Fatalf("deploy %d %v", status, result)
	}
	id := int64(result["job"].(map[string]any)["id"].(float64))
	body := `{"id":` + fmtInt(id) + `,"status":"deployed","reason":"healthy"}`
	for range 2 {
		status, result = workerRequest(t, handler, token, "POST", "/api/v1/hosts/nas/deploy-result", body)
		if status != 200 {
			t.Fatalf("deploy result %d %v", status, result)
		}
	}
	status, _ = workerRequest(t, handler, token, "POST", "/api/v1/hosts/nas/deploy-result", `{"id":`+fmtInt(id)+`,"status":"failed","reason":"interrupted"}`)
	if status != 409 {
		t.Fatalf("changed result %d", status)
	}
}

func TestCanaryKeepsOtherHostsOnPreviousRelease(t *testing.T) {
	_, db, _ := workerFixture(t)
	service := hostd.Service{DB: db, Assignments: func() (*fleet.Assignments, error) {
		return &fleet.Assignments{ReleaseCanaryHosts: []string{"nas"}}, nil
	}}
	old, newRevision := strings.Repeat("a", 40), strings.Repeat("b", 40)
	body := func(revision string) map[string]any {
		return map[string]any{"registry": "registry.example", "tag": revision[:12], "digest": "sha256:" + strings.Repeat("f", 64), "revision": revision}
	}
	if _, err := service.Register(context.Background(), body(old)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO host_image_results(host,tag,flavor,digest,status)VALUES('nas','aaaaaaaaaaaa','','sha256:" + strings.Repeat("f", 64) + "','success')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO hosts(host, heartbeat_at, hostd_health) VALUES ('nas', now(), '{"cycle":{}}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(context.Background(), body(newRevision)); err != nil {
		t.Fatal(err)
	}
	other, err := service.Desired(context.Background(), "other", "", old)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(other)
	var response map[string]any
	_ = json.Unmarshal(raw, &response)
	if response["desired"].(map[string]any)["revision"] != old {
		t.Fatal("unverified release reached other host", response)
	}
	if _, err = service.Register(context.Background(), body(newRevision)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow("SELECT count(*)FROM release_rollouts WHERE status='canary'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate rollout %d %v", count, err)
	}
}

func TestMonitorHostIgnoresLegacyRepositories(t *testing.T) {
	a := &fleet.Assignments{Output: map[string]any{"repositories": map[string]any{"mofua-suki/issue-desk": "other"}}, Implementers: map[string][]string{"mofua-suki/issue-desk": {"other"}}}
	if hosts.MonitorHost(nil) != "" || hosts.MonitorHost(a) != "" {
		t.Fatal("legacy configuration enabled monitoring")
	}
	a.MonitorHost = "nas"
	if hosts.MonitorHost(a) != "nas" {
		t.Fatal("legacy repository overrode monitor host")
	}
}

func TestCanaryUsesOnlyConfiguredOrder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []string
		want       string
	}{
		{"legacy_only", nil, ""}, {"configured_order", []string{"other", "nas"}, "other"}, {"configured_host", []string{"nas"}, "nas"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db, _ := workerFixture(t)
			service := hostd.Service{DB: db, Assignments: func() (*fleet.Assignments, error) {
				return &fleet.Assignments{ReleaseCanaryHosts: tc.candidates, Implementers: map[string][]string{"mofua-suki/issue-desk": {"nas"}}, Output: map[string]any{"canaryHosts": map[string]any{"mofua-suki/issue-desk": "nas"}, "repositories": map[string]any{"mofua-suki/issue-desk": "nas"}}}, nil
			}}
			old, next := strings.Repeat("a", 40), strings.Repeat("b", 40)
			body := func(revision string) map[string]any {
				return map[string]any{"registry": "registry.example", "tag": revision[:12], "digest": "sha256:" + strings.Repeat("f", 64), "revision": revision}
			}
			if _, err := service.Register(context.Background(), body(old)); err != nil {
				t.Fatal(err)
			}
			for _, host := range []string{"nas", "other"} {
				if _, err := db.Exec(`INSERT INTO hosts(host, heartbeat_at, hostd_health) VALUES ($1, now(), '{"cycle":{}}')`, host); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO host_image_results(host,tag,flavor,digest,status) VALUES ($1,'aaaaaaaaaaaa','',$2,'success')`, host, "sha256:"+strings.Repeat("f", 64)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := service.Register(context.Background(), body(next)); err != nil {
				t.Fatal(err)
			}
			var got string
			err := db.QueryRow(`SELECT canary FROM release_rollouts WHERE status='canary'`).Scan(&got)
			if tc.want == "" && errors.Is(err, sql.ErrNoRows) {
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("canary=%q want=%q err=%v", got, tc.want, err)
			}
		})
	}
}

func TestImageResultHistory(t *testing.T) {
	handler, db, _ := workerFixture(t)
	token := "idh_" + strings.Repeat("b", 43)
	if _, err := db.Exec("INSERT INTO host_image_tokens(host,hash)VALUES('nas',$1)", auth.Hash(token)); err != nil {
		t.Fatal(err)
	}
	base := `"tag":"aaaaaaaaaaaa","flavor":"","digest":"sha256:` + strings.Repeat("b", 64) + `","status":"failed"`
	send := func(extra string) int {
		status, _ := workerRequest(t, handler, token, "POST", "/api/v1/hosts/nas/image-result", `{`+base+extra+`}`)
		return status
	}
	if got := send(""); got != 200 {
		t.Fatal(got)
	}
	if got := send(`,"stage":"docker","reason":"token=secret123","detail":"password=secret456","selfCheckFailed":["api_key=secret789"],"fromTag":"bbbbbbbbbbbb","startedAt":"2026-11-01T00:00:00Z"`); got != 200 {
		t.Fatal(got)
	}
	var count int
	var reason, detail, checks string
	if err := db.QueryRow(`SELECT reason,detail,self_check_failed::text FROM host_image_history WHERE stage='docker'`).Scan(&reason, &detail, &checks); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{reason, detail, checks} {
		if strings.Contains(v, "secret") || !strings.Contains(v, "REDACTED") {
			t.Fatal("unredacted", v)
		}
	}
	for _, extra := range []string{`,"stage":"bad"`, `,"reason":3`, `,"detail":"` + strings.Repeat("x", 201) + `"`, `,"selfCheckFailed":[3]`, `,"selfCheckFailed":null`, `,"fromTag":"bad"`, `,"startedAt":"bad"`, `,"reason":"a\u0000b"`, `,"detail":"a\u0000b"`, `,"selfCheckFailed":["a\u0000b"]`, `,"startedAt":"0000-01-01T00:00:00Z"`, `,"startedAt":"0001-01-01T00:00:00+01:00"`, `,"startedAt":"9999-12-31T23:59:59-01:00"`} {
		body := `{` + strings.Replace(base, "aaaaaaaaaaaa", "dddddddddddd", 1) + extra + `}`
		if got, _ := workerRequest(t, handler, token, "POST", "/api/v1/hosts/nas/image-result", body); got != 400 {
			t.Fatalf("%s: %d", extra, got)
		}
		var results, history int
		if err := db.QueryRow(`SELECT (SELECT count(*) FROM host_image_results), (SELECT count(*) FROM host_image_history)`).Scan(&results, &history); err != nil || results != 1 || history != 2 {
			t.Fatalf("%s: results=%d history=%d err=%v", extra, results, history, err)
		}
	}
	if err := db.QueryRow(`SELECT count(*) FROM host_image_history`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("history %d %v", count, err)
	}
	if _, err := db.Exec(`ALTER TABLE host_image_history ADD CONSTRAINT reject_history CHECK (tag <> 'cccccccccccc')`); err != nil {
		t.Fatal(err)
	}
	body := `{"tag":"cccccccccccc","flavor":"","digest":"sha256:` + strings.Repeat("b", 64) + `","status":"failed"}`
	if got, _ := workerRequest(t, handler, token, "POST", "/api/v1/hosts/nas/image-result", body); got < 500 || got >= 600 {
		t.Fatalf("expected 5xx storage failure, got %d", got)
	}
	if err := db.QueryRow(`SELECT count(*) FROM host_image_results WHERE tag='cccccccccccc'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("atomicity %d %v", count, err)
	}
}
