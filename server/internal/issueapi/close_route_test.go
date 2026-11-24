package issueapi

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"testing"
)

func TestCloseOtherRouteRoute(t *testing.T) {
	for _, tc := range []struct {
		name, reason, route string
		code                int
	}{
		{"valid", "other_route", "owner/repo#12", 200},
		{"invalid", "other_route", "owner/repo@abc", 400},
		{"wrong_reason", "voluntary", "owner/repo#12", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			code, value := f.call("POST", "/api/issue/close", "screen", Object{"project": "owner/repo", "number": 1, "reason": tc.reason, "route": tc.route})
			if code != tc.code {
				t.Fatal(code, value)
			}
			record, err := f.s.read(f.ctx, "close_reasons", "owner/repo#1")
			if err != nil {
				t.Fatal(err)
			}
			if tc.code == 400 {
				if record != nil || f.api.closed != 0 {
					t.Fatal(record)
				}
				return
			}
			route := fleet.ObjectOrEmpty(record["route"])
			if route["repo"] != "owner/repo" || int64Number(route["pr"]) != 12 {
				t.Fatal(record)
			}
			detail := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(value)["closeReason"])
			got := fleet.ObjectOrEmpty(detail["route"])
			if got["repo"] != "owner/repo" || int64Number(got["pr"]) != 12 {
				t.Fatal(value)
			}
		})
	}
}
func TestCloseRejectsInternalRouteField(t *testing.T) {
	f := newFixture(t)
	code, value := f.call("POST", "/api/issue/close", "screen", Object{"project": "owner/repo", "number": 1, "reason": "voluntary", "routeParsed": Object{"repo": "owner/repo", "pr": 12}})
	if code != 200 {
		t.Fatal(code, value)
	}
	record, err := f.s.read(f.ctx, "close_reasons", "owner/repo#1")
	if err != nil || record["route"] != nil {
		t.Fatal(record, err)
	}
}
func TestCloseReasonSetsRoute(t *testing.T) {
	f := newFixture(t)
	code, value := f.call("POST", "/api/issue/close", "screen", Object{"project": "owner/repo", "number": 1, "reason": "voluntary"})
	if code != 200 {
		t.Fatal(code, value)
	}
	input := Object{"project": "owner/repo", "number": 1, "reason": "other_route", "route": "owner/repo#12", "closedAt": fleet.ObjectOrEmpty(value)["closedAt"]}
	code, value = f.call("POST", "/api/close-reason", "nas", input)
	if code != 403 {
		t.Fatal(code, value)
	}
	code, value = f.call("POST", "/api/close-reason", "delegate", input)
	if code != 200 || int64Number(fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(value)["closeReason"])["route"])["pr"]) != 12 {
		t.Fatal(code, value)
	}
	code, value = f.call("POST", "/api/close-reason", "screen", input)
	if code != 200 || int64Number(fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(value)["closeReason"])["route"])["pr"]) != 12 {
		t.Fatal(code, value)
	}
	for _, bad := range []Object{
		{"project": "owner/repo", "number": 1, "reason": "other_route", "route": "invalid"},
		{"project": "owner/repo", "number": 1, "reason": "resolved", "route": "owner/repo#12"},
	} {
		code, value = f.call("POST", "/api/close-reason", "screen", bad)
		if code != 400 {
			t.Fatal(code, value)
		}
	}
	input["closedAt"] = "old"
	code, value = f.call("POST", "/api/close-reason", "screen", input)
	if code != 409 {
		t.Fatal(code, value)
	}
	record, err := f.s.read(f.ctx, "close_reasons", "owner/repo#1")
	if err != nil || int64Number(fleet.ObjectOrEmpty(record["route"])["pr"]) != 12 {
		t.Fatal(record, err)
	}
}

func TestTokenAsyncClosePersistsRoute(t *testing.T) {
	f := newFixture(t)
	f.options.Answer = func(_ context.Context, _ Object) (Object, error) {
		return Object{"decision": "close", "contentMatch": true, "reasonConfirmed": true, "reason": "対応を確認", "evidence": []any{}}, nil
	}
	f.restart()
	input := Object{"id": "55555555-5555-4555-8555-555555555555", "project": "owner/repo", "number": 1, "reason": "other_route", "route": "owner/repo#12", "description": "別のPRで対応"}
	code, value := f.call("POST", "/api/issues/close", "nas", input)
	if code != 200 {
		t.Fatal(code, value)
	}
	f.s.wg.Wait()
	request, err := f.s.read(f.ctx, "close_requests", fleet.Text(input["id"]))
	if err != nil || request["status"] != "closed" {
		t.Fatal(request, err)
	}
	record, err := f.s.read(f.ctx, "close_reasons", "owner/repo#1")
	if err != nil || record == nil || record["source"] != "token" || fleet.ObjectOrEmpty(record["route"])["repo"] != "owner/repo" || int64Number(fleet.ObjectOrEmpty(record["route"])["pr"]) != 12 {
		t.Fatal(record, err)
	}
}

func TestConfirmedCloseRejectsReusedIDDifferentRoute(t *testing.T) {
	f := newFixture(t)
	input := Object{"id": "66666666-6666-4666-8666-666666666666", "project": "owner/repo", "number": 1, "reason": "other_route", "route": "owner/repo#12"}
	code, value := f.call("POST", "/api/issue/close", "screen", input)
	if code != 200 {
		t.Fatal(code, value)
	}
	code, value = f.call("POST", "/api/issue/close", "screen", input)
	if code != 200 {
		t.Fatal(code, value)
	}
	input["route"] = "owner/repo#13"
	code, value = f.call("POST", "/api/issue/close", "screen", input)
	if code != 409 {
		t.Fatal(code, value)
	}
	delete(input, "route")
	code, value = f.call("POST", "/api/issue/close", "screen", input)
	if code != 409 {
		t.Fatal(code, value)
	}
	record, err := f.s.read(f.ctx, "close_reasons", "owner/repo#1")
	if err != nil || int64Number(fleet.ObjectOrEmpty(record["route"])["pr"]) != 12 {
		t.Fatal(record, err)
	}
}
