package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hosts"
	"issue-desk-intake/server/internal/screenapi"
)

func TestImageResultsPostgresViewAndStallAlerts(t *testing.T) {
	handler, db, token := workerFixtureOptions(t, true, nil)
	_, err := db.Exec(`INSERT INTO hosts(host,heartbeat_at,ready,revision,hostd_health) VALUES ('nas',now(),true,$1,'{"cycle":{"result":"failed","stage":"swap-start","exceptionType":null,"at":"2026-11-01T00:00:00Z","receivedAt":1000,"failures":1,"firstFailureAt":1000,"lastSuccessAt":null,"detail":"docker_exit"}}')`, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		host, tag, status string
		age               int
	}{
		{"nas", "bbbbbbbbbbbb", "failed", 1}, {"nas", "cccccccccccc", "failed", 2},
		{"other", "bbbbbbbbbbbb", "success", 3}, {"other", "cccccccccccc", "success", 4},
	} {
		if _, err = db.Exec(`INSERT INTO host_image_results(host,tag,flavor,digest,status,at) VALUES($1,$2,'','sha256:x',$3,now()-$4*interval '1 minute')`, row.host, row.tag, row.status, row.age); err != nil {
			t.Fatal(err)
		}
	}
	check := func(view map[string]any) {
		t.Helper()
		for _, v := range fleet.List(view["hosts"]) {
			row := fleet.Object(v)
			if row["name"] != "nas" {
				continue
			}
			image := fleet.Object(row["image"])
			results := fleet.List(image["results"])
			if len(results) != 2 || fleet.Object(results[0])["tag"] != "bbbbbbbbbbbb" || fleet.Object(results[1])["tag"] != "cccccccccccc" || len(fleet.Strings(fleet.Object(results[0])["succeededOn"])) != 1 || fleet.Strings(fleet.Object(results[0])["succeededOn"])[0] != "other" || len(fleet.Strings(image["failedTags"])) != 2 || fleet.Object(image["lastFailure"])["stage"] != "swap-start" {
				t.Fatalf("image: %#v", image)
			}
			return
		}
		t.Fatal("nas missing from view")
	}
	status, view := workerRequest(t, handler, token, "GET", "/api/hosts", "")
	if status != 200 {
		t.Fatal(status, view)
	}
	check(view)
	// Use the assignments shipped with the application for the initial list.
	raw, err := os.ReadFile("../../../assignments.json")
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(data)
	if err != nil {
		t.Fatal(err)
	}
	delta := screenapi.NewDelta(screenapi.DeltaOptions{})
	first, err := delta.Respond(context.Background(), "screen", nil, func(ctx context.Context) (screenapi.Snapshot, error) {
		v, e := (&hosts.Service{DB: db, Now: time.Now}).View(ctx, a)
		return screenapi.Snapshot{Hosts: v}, e
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(first["hosts"])
	if err != nil {
		t.Fatal(err)
	}
	var initial map[string]any
	if err = json.Unmarshal(encoded, &initial); err != nil {
		t.Fatal(err)
	}
	check(initial)
	getAlerts := func() []map[string]any {
		t.Helper()
		r := httptest.NewRequest("GET", "http://localhost:8787/api/stalls/alerts", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var alerts []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &alerts); err != nil || w.Code != 200 {
			t.Fatalf("alerts HTTP %d: %s", w.Code, w.Body.String())
		}
		return alerts
	}
	found := false
	for _, alert := range getAlerts() {
		if alert["id"] == "host-drift-update-failing-nas" {
			found = true
			if !strings.Contains(fleet.Text(alert["detail"]), "other") || !strings.Contains(fleet.Text(alert["detail"]), "swap-start") {
				t.Fatal(alert)
			}
		}
	}
	if !found {
		t.Fatal("missing repeated failure alert")
	}
	if _, err = db.Exec(`UPDATE host_image_results SET status='success' WHERE host='nas' AND tag='bbbbbbbbbbbb'`); err != nil {
		t.Fatal(err)
	}
	for _, alert := range getAlerts() {
		if alert["id"] == "host-drift-update-failing-nas" {
			t.Fatal("alert remained after success", alert)
		}
	}
}

func TestImageFailureHistoryThroughAPI(t *testing.T) {
	handler, db, workerToken := workerFixture(t)
	imageToken := "idh_" + strings.Repeat("b", 43)
	if _, err := db.Exec("INSERT INTO host_image_tokens(host,hash) VALUES('nas',$1)", auth.Hash(imageToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO hosts(host,heartbeat_at,ready,revision,hostd_health) VALUES ('nas',now(),true,$1,'{"cycle":{"result":"success","stage":"complete","exceptionType":null,"at":"2026-11-01T00:00:00Z","receivedAt":1000,"failures":0,"firstFailureAt":null,"lastSuccessAt":1000}}')`, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	body := `{"tag":"aaaaaaaaaaaa","flavor":"","digest":"sha256:` + strings.Repeat("b", 64) + `","status":"failed","stage":"docker","reason":"network","detail":"failed","selfCheckFailed":["check"]}`
	status, response := workerRequest(t, handler, imageToken, "POST", "/api/v1/hosts/nas/image-result", body)
	if status != 200 {
		t.Fatal(status, response)
	}
	status, response = workerRequest(t, handler, workerToken, "GET", "/api/hosts", "")
	if status != 200 {
		t.Fatal(status, response)
	}
	for _, raw := range fleet.List(response["hosts"]) {
		row := fleet.Object(raw)
		if row["name"] != "nas" {
			continue
		}
		failure := fleet.Object(fleet.Object(row["image"])["lastFailure"])
		if failure["reason"] != "network" || failure["stage"] != "docker" || failure["detail"] != "failed" || len(fleet.List(failure["selfCheckFailed"])) != 1 {
			t.Fatalf("history missing: %#v", failure)
		}
		return
	}
	t.Fatal("nas missing")
}
