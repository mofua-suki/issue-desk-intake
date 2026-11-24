package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

func TestHostdDrainAcceptsNativeNanosecondExpiry(t *testing.T) {
	handler, db, pc := workerFixture(t)
	token := "idh_" + strings.Repeat("n", 43)
	if _, e := db.Exec("INSERT INTO host_image_tokens(host,hash)VALUES('nas',$1)", auth.Hash(token)); e != nil {
		t.Fatal(e)
	}
	if status, result := workerRequest(t, handler, pc, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":1}`); status != 200 {
		t.Fatal("PCの登録", status, result)
	}
	until := time.Unix(time.Now().Add(time.Hour).Unix(), 123456789).UTC()
	for _, nanos := range []int{0, 100000000, 123000000, 123456000, 123456700, 123456780, 123456789} {
		want := time.Unix(until.Unix(), int64(nanos)).UTC()
		body, _ := json.Marshal(map[string]any{"until": want.Format(time.RFC3339Nano)})
		status, result := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas/drain", string(body))
		if status != 200 {
			t.Fatal(want, status, result)
		}
		mark := result["drain"].(map[string]any)
		got, e := time.Parse(time.RFC3339Nano, mark["until"].(string))
		if e != nil || !got.Equal(want.Truncate(time.Millisecond)) {
			t.Fatal("停止期限を変更した", mark, e)
		}
	}
	bad := until.Format("2006-01-02T15:04:05") + ".1234567890Z"
	body, _ := json.Marshal(map[string]any{"until": bad})
	if status, _ := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas/drain", string(body)); status != 400 {
		t.Fatal("桁数上限を超えた時刻を受理した", status)
	}
}

func TestIndefiniteManualDrainSurvivesHostd(t *testing.T) {
	handler, db, pc := workerFixture(t)
	token := "idh_" + strings.Repeat("n", 43)
	if _, err := db.Exec("INSERT INTO host_image_tokens(host,hash)VALUES('nas',$1)", auth.Hash(token)); err != nil {
		t.Fatal(err)
	}
	if status, result := workerRequest(t, handler, pc, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":1}`); status != 200 {
		t.Fatal(status, result)
	}
	for _, body := range []string{`{"reason":"maintenance","indefinite":"true"}`, `{"reason":"maintenance","indefinite":true,"until":"2099-01-01T00:00:00Z"}`, `{"reason":"maintenance","unknown":true}`} {
		if status, result := workerRequest(t, handler, pc, "POST", "/api/hosts/nas/drain", body); status != 400 {
			t.Fatal(body, status, result)
		}
	}
	if status, result := workerRequest(t, handler, pc, "POST", "/api/hosts/nas/drain", `{"reason":"maintenance","indefinite":true}`); status != 200 {
		t.Fatal(status, result)
	}
	for _, method := range []string{"PUT", "DELETE"} {
		if status, result := workerRequest(t, handler, token, method, "/api/v1/hosts/nas/drain", `{}`); status != 200 {
			t.Fatal(method, status, result)
		}
		var raw []byte
		if err := db.QueryRow("SELECT drain FROM hosts WHERE host='nas'").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var mark map[string]any
		if err := json.Unmarshal(raw, &mark); err != nil || mark["indefinite"] != true {
			t.Fatal(string(raw), err)
		}
	}
}

func TestManualDrainDefaultForOmittedAndNullUntil(t *testing.T) {
	handler, _, pc := workerFixture(t)
	if status, result := workerRequest(t, handler, pc, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":1}`); status != 200 {
		t.Fatal(status, result)
	}
	for _, body := range []string{`{"reason":"test"}`, `{"reason":"test","until":null}`} {
		before := time.Now()
		status, result := workerRequest(t, handler, pc, "POST", "/api/hosts/nas/drain", body)
		if status != 200 {
			t.Fatal(status, result)
		}
		mark := result["drain"].(map[string]any)
		expiry, err := time.Parse(time.RFC3339Nano, mark["until"].(string))
		if err != nil || expiry.Before(before.Add(149*time.Minute)) || expiry.After(time.Now().Add(151*time.Minute)) {
			t.Fatal(mark, err)
		}
	}
}
