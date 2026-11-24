package projects

import (
	"context"
	"encoding/json"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func assertProjectDisplayAgreement(t *testing.T, f *fixture, mutation Object) {
	t.Helper()
	single := projectGET(f, "/api/projects/"+id)
	var one Object
	if err := json.Unmarshal(single.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	rows := decodeProjectList(t, projectGET(f, "/api/projects"))
	if single.Code != 200 || len(rows) != 1 || !reflect.DeepEqual(one, rows[0]) {
		t.Fatal("single/list mismatch", one, rows)
	}
	for _, key := range []string{"status", "actions", "approvalValid", "creationLocked", "finishingTurn", "creationUnavailable"} {
		if !reflect.DeepEqual(one[key], mutation[key]) {
			t.Fatalf("mutation differs for %s: %v / %v", key, mutation, one)
		}
	}
}

func TestProjectPresentationNewMutationResponses(t *testing.T) {
	f := newFixture(t)
	type turn struct{ finish chan struct{} }
	started := make(chan turn, 1)
	f.options.ConfigFile = ""
	f.options.Answer = func(ctx context.Context, _ Object) (Object, error) {
		next := turn{make(chan struct{})}
		started <- next
		select {
		case <-next.finish:
			return answer(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.restart()
	for _, path := range []string{"/api/projects", "/api/projects/" + id + "/chat"} {
		requestID := id
		if path != "/api/projects" {
			requestID = otherProjectID
		}
		code, mutation := f.call(path, Object{"id": requestID, "text": "新しい相談"})
		if code != 202 {
			t.Fatal(code, mutation)
		}
		next := projectReceive(t, started)
		assertProjectDisplayAgreement(t, f, mutation)
		close(next.finish)
		if err := f.s.WaitAccepted(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	p, err := f.s.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	code, mutation := f.call("/api/projects/"+id+"/approve", Object{"fingerprint": p["fingerprint"]})
	if code != 200 {
		t.Fatal(code, mutation)
	}
	assertProjectDisplayAgreement(t, f, mutation)
}

func TestProjectPresentationOptionalFieldsAndFilingActions(t *testing.T) {
	f := newFixture(t) // DeskNumber is nil in the actual default fixture
	for _, scenario := range []string{"missing-definition", "requested", "file-ready", "host-waiting", "filed", "missing-registration"} {
		t.Run(scenario, func(t *testing.T) {
			p := presentationRecord(id, "registered")
			switch scenario {
			case "missing-definition":
				p["status"], p["definition"], p["approval"], p["creation"], p["filing"] = "draft", nil, nil, nil, nil
			case "requested":
				p["status"], p["creation"] = "creation_failed", Object{"steps": Object{"requested": Object{"at": "before"}}}
			case "file-ready":
				fleet.ObjectOrEmpty(p["filing"])["status"] = "failed"
			case "host-waiting":
				fleet.ObjectOrEmpty(p["filing"])["status"] = "host_waiting"
			case "missing-registration":
				p["creation"] = nil
			}
			savePresentationRecord(t, f, p, false)
			f.s.mu.Lock()
			expected := referencePresent09f2c006(f.s, p)
			f.s.mu.Unlock()
			rows := decodeProjectList(t, projectGET(f, "/api/projects"))
			if len(rows) != 1 || !reflect.DeepEqual(rows[0], clone(expected)) {
				t.Fatal("optional/default rendering differs", rows, expected)
			}
			assertProjectDisplayAgreement(t, f, rows[0])
			after, err := f.s.Snapshot(context.Background(), id)
			if err != nil || !reflect.DeepEqual(after, clone(p)) {
				t.Fatal("rendering changed saved fields", after, p, err)
			}
		})
	}
}

func TestProjectListAuthenticationAndValidation(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name, path, credential string
		status                 int
	}{
		{"anonymous", "/api/projects", "", 401},
		{"invalid-bearer-no-cookie-fallback", "/api/projects", "bearer", 401},
		{"invalid-id", "/api/projects/not-a-uuid", "screen", 404},
		{"unknown-route", "/api/projects/" + id + "/unknown", "screen", 404},
		{"missing-project", "/api/projects/" + id, "screen", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://127.0.0.1:8787"+tc.path, nil)
			if tc.credential != "" {
				r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
			}
			if tc.credential == "bearer" {
				r.Header.Set("Authorization", "Bearer invalid-fixture")
			}
			w := httptest.NewRecorder()
			f.mux.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
	if code, _ := f.call("/api/projects/"+id+"/chat", Object{"text": "変更"}, map[string]string{"X-CSRF-Token": "invalid"}); code != 403 {
		t.Fatal("CSRF", code)
	}
	if code, _ := f.call("/api/projects", Object{"id": id, "text": ""}); code != 400 {
		t.Fatal("input validation", code)
	}
}
