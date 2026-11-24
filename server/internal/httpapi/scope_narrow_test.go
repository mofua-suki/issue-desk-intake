package httpapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestWorkerScopeActualAndAddValidation(t *testing.T) {
	handler, db, token := workerFixture(t)
	var id int64
	if err := db.QueryRow(`INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,scope,view) VALUES('owner/repo',1,'running','nas',1,now()+interval '1 hour',ARRAY['server/','client/'],'{}') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/jobs/" + fmtInt(id) + "/scope"
	for _, body := range []string{`{"epoch":1,"add":["x"],"actual":["server/a"]}`, `{"epoch":1,"actual":["*"]}`, `{"epoch":1,"actual":["outside"]}`, `{"epoch":1,"actual":[` + strings.Repeat(`"server/a",`, 200) + `"server/b"]}`} {
		status, value := workerRequest(t, handler, token, "POST", path, body)
		if status != 400 {
			t.Fatalf("%s: %d %v", body, status, value)
		}
	}
	for _, body := range []string{`{"epoch":1,"actual":"x"}`, `{"epoch":1,"actual":[]}`, `{"epoch":1,"actual":[1]}`, `{"epoch":1,"add":"x"}`, `{"epoch":1,"add":[]}`, `{"epoch":1,"add":[1]}`} {
		status, value := workerRequest(t, handler, token, "POST", path, body)
		key := "actual"
		if strings.Contains(body, `"add"`) {
			key = "add"
		}
		if status != 400 || value["error"] != key+"はパスの配列です。" {
			t.Fatalf("invalid %s: %d %v", body, status, value)
		}
	}
	status, value := workerRequest(t, handler, token, "POST", path, `{"epoch":2,"actual":["server/a"]}`)
	if status != 409 || value["code"] != "lease_lost" {
		t.Fatalf("fence %d %v", status, value)
	}
	status, value = workerRequest(t, handler, token, "POST", path, `{"epoch":1,"actual":["SERVER/a","server/b","SERVER/a"]}`)
	if status != 200 || value["result"] != "granted" {
		t.Fatalf("actual %d %v", status, value)
	}
	var data []byte
	if err := db.QueryRow(`SELECT data FROM job_events WHERE job_id=$1 AND kind='scope_narrowed'`, id).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var event map[string][]string
	if err := json.Unmarshal(data, &event); err != nil || !reflect.DeepEqual(event["actual"], []string{"SERVER/a", "server/b", "SERVER/a"}) || !reflect.DeepEqual(event["after"], []string{"SERVER/a", "server/b"}) {
		t.Fatalf("event %s: %v", data, err)
	}
	status, value = workerRequest(t, handler, token, "POST", path, `{"epoch":1,"add":["client/a"]}`)
	if status != 200 || value["result"] != "granted" {
		t.Fatalf("add %d %v", status, value)
	}
}

func TestWorkerScopeOverLimitJSONNullResult(t *testing.T) {
	handler, db, token := workerFixture(t)
	var id int64
	if err := db.QueryRow(`INSERT INTO jobs(repo,issue,status,lease_host,lease_epoch,lease_expires_at,scope,scope_source,view,result) VALUES('owner/repo',1,'running','nas',1,now()+interval '1 hour',ARRAY['a','b','c','d'],'declared','{}','null'::jsonb) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	status, value := workerRequest(t, handler, token, "POST", "/api/v1/jobs/"+fmtInt(id)+"/scope", `{"epoch":1,"add":["e","f","g","h","i"]}`)
	if status != 409 || value["code"] != "lease_lost" || value["result"] != nil {
		t.Fatalf("response: %d %v", status, value)
	}
	var typ, storedStatus, message string
	var released bool
	if err := db.QueryRow(`SELECT jsonb_typeof(result),status,result->>'error',lease_expires_at IS NULL AND hold_reason IS NULL AND scope=ARRAY['a','b','c','d'] FROM jobs WHERE id=$1`, id).Scan(&typ, &storedStatus, &message, &released); err != nil || typ != "object" || storedStatus != "stopped" || message == "" || !released {
		t.Fatalf("stored: %s %s %s %v %v", typ, storedStatus, message, released, err)
	}
}
