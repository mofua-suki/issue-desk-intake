package httpapi

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/auth"
)

func TestHostViewAndDrainKeepPlainPCAndLocalCLIContract(t *testing.T) {
	var local *WorkerRouteOptions
	handler, _, token := workerFixtureOptions(t, false, func(o *WorkerRouteOptions) { local = o })
	status, view := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{"ready":true,"capacity":1}`)
	if status != 200 {
		t.Fatal(status, view)
	}
	for _, request := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/hosts", "", 200},
		{"POST", "/api/hosts/nas/drain", `{"reason":"既存PCの配置"}`, 200},
		{"DELETE", "/api/hosts/nas/drain", "", 200},
		{"GET", "/api/hosts/nas/hostd-command", "", 403},
	} {
		status, view = workerRequest(t, handler, token, request.method, request.path, request.body)
		if status != request.status {
			t.Fatal(request.path, status, view)
		}
	}
	for i, request := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/hosts", "", 200},
		{"POST", "/api/hosts/nas/drain", `{"reason":"NASの配置"}`, 200},
		{"DELETE", "/api/hosts/nas/drain", "", 200},
		{"GET", "/api/hosts/nas/hostd-command", "", 403},
		{"POST", "/api/hosts/nas/hostd-command", `{}`, 403},
	} {
		key := fmt.Sprintf("local-fixture-%d", i)
		if _, err := local.AppDB.Exec("INSERT INTO cli_keys(hash,expires)VALUES(?,?)", auth.Hash(key), time.Now().Add(time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(request.method, "http://localhost:8787"+request.path, strings.NewReader(request.body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("X-Desk-Key", key)
		r.Header.Set("Origin", "http://localhost:8787")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != request.status {
			t.Fatal(request.path, w.Code, w.Body.String())
		}
	}
}
