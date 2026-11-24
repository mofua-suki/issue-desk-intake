package hostd

import (
	"encoding/json"
	"testing"
	"time"
)

func object(raw string) map[string]any {
	var v map[string]any
	_ = json.Unmarshal([]byte(raw), &v)
	return v
}
func TestCycleOrderingAndFailures(t *testing.T) {
	first := object(`{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","phase":"start","result":"failed","stage":"recover","exceptionType":null,"at":"2026-11-01T00:00:00Z"}`)
	if !validCycle(first) {
		t.Fatal("valid start rejected")
	}
	old := nextCycle(first, nil, 100)
	finish := object(`{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","phase":"finish","result":"success","stage":"complete","exceptionType":null,"at":"2026-11-01T00:00:01Z"}`)
	old = nextCycle(finish, old, 90)
	if old["receivedAt"] != float64(100) || old["failures"] != 0 {
		t.Fatalf("clock regression %v", old)
	}
	if got := nextCycle(first, old, 110); got["result"] != "success" {
		t.Fatal("late start overwrote finish")
	}
	bad := object(`{"result":"success","stage":"client","exceptionType":null,"at":"2026-11-01T00:00:00Z"}`)
	if validCycle(bad) {
		t.Fatal("incomplete success accepted")
	}
	bad["result"] = "failed"
	bad["detail"] = "https://secret.example"
	if validCycle(bad) {
		t.Fatal("URL leaked into diagnostic")
	}
}
func TestCommandsAreImmutableAndPinChangesOnSuccess(t *testing.T) {
	now := time.Unix(100, 0)
	state := object(`{"command":{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","kind":"pin","tag":"0123456789ab","by":"screen"},"pin":null,"results":[]}`)
	body := object(`{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ok":true,"reason":"done","at":"2026-11-01T00:00:00Z"}`)
	saved, err := completeCommand(state, body, now)
	if err != nil || saved["pin"] == nil || saved["command"] != nil {
		t.Fatalf("complete %v %v", saved, err)
	}
	if _, err = completeCommand(saved, body, now); err != nil {
		t.Fatal("idempotent result rejected", err)
	}
	body["ok"] = false
	body["reason"] = "failed"
	if _, err = completeCommand(saved, body, now); err == nil {
		t.Fatal("conflicting result accepted")
	}
}
func TestLiveResultRejectsCredentialsAndPreservesNumericMetrics(t *testing.T) {
	value := object(`{"pass":true,"summary":"healthy","metrics":{"latency":1.2},"failures":[]}`)
	if !validLive(value) {
		t.Fatal("valid metrics rejected")
	}
	value["summary"] = "token=secret"
	if validLive(value) {
		t.Fatal("credential text accepted")
	}
}
