package projects

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/contract"
)

const otherProjectID = "22222222-2222-4222-8222-222222222222"

func projectGET(f *fixture, path string) *httptest.ResponseRecorder {
	return projectGETHandler(f.mux, path)
}

func projectGETHandler(handler http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://127.0.0.1:8787"+path, nil)
	r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func projectReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	// Deadlock guard only: correctness is determined by channel ordering, never
	// by comparing elapsed time with a performance threshold.
	case <-time.After(30 * time.Second):
		t.Fatal("project presentation event did not complete (deadlock guard)")
		var zero T
		return zero
	}
}

func savePresentationRecord(t *testing.T, f *fixture, p Object, busy bool) {
	t.Helper()
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	if err := f.s.save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	f.s.running[fleet.Text(p["id"])] = busy
}

func presentationRecord(projectID, status string) Object {
	d := definition()
	return Object{"id": projectID, "provider": "codex", "consultationProvider": "codex", "status": status, "definition": d,
		"approval": Object{"fingerprint": Fingerprint(d)}, "revision": 7,
		"createdAt": projectID, "messages": []any{Object{"id": id, "requestId": id, "role": "user", "text": "相談本文 <>&\n" + strings.Repeat("長い会話", 100)}},
		"questions": []any{}, "extension": Object{"nested": []any{true, nil, "保持"}},
		"creation": Object{"steps": Object{"registered": Object{"name": "owner/demo"}}},
		"filing":   Object{"status": "filed", "issues": []any{Object{"project": "owner/demo", "number": 12, "url": "https://gitea.invalid/12", "desk": "old"}, Object{"project": "owner/demo", "number": 13}}}}
}

func decodeProjectList(t *testing.T, w *httptest.ResponseRecorder) []Object {
	t.Helper()
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get(contract.Header) != contract.Version {
		t.Fatalf("list response status/headers: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	var out []Object
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestProjectPresentationAllStatesAndCallers(t *testing.T) {
	f := newFixture(t)
	f.options.DeskNumber = func(_ string, n int64) any {
		if n == 12 {
			return 5374
		}
		return nil
	}
	f.restart()
	if got := projectGET(f, "/api/projects"); strings.TrimSpace(got.Body.String()) != "[]" {
		t.Fatal("empty list", got.Body.String())
	}
	states := []string{"draft", "waiting", "thinking", "error", "ready", "create_approved", "creating", "creation_failed", "creation_blocked", "registered"}
	for _, status := range states {
		for _, busy := range []bool{false, true} {
			for _, available := range []bool{false, true} {
				for _, valid := range []bool{false, true} {
					name := fmt.Sprintf("%s/busy=%t/available=%t/approved=%t", status, busy, available, valid)
					t.Run(name, func(t *testing.T) {
						p := presentationRecord(id, status)
						if !valid {
							p["approval"] = nil
						}
						if status != "registered" {
							p["creation"] = nil
						}
						savePresentationRecord(t, f, p, busy)
						f.s.mu.Lock()
						f.s.o.ConfigFile = ""
						if available {
							f.s.o.ConfigFile = "fixture"
						}
						expected := referencePresent09f2c006(f.s, p)
						before, err := f.s.load(context.Background(), id)
						f.s.mu.Unlock()
						if err != nil {
							t.Fatal(err)
						}
						rows := decodeProjectList(t, projectGET(f, "/api/projects"))
						if len(rows) != 1 || !reflect.DeepEqual(rows[0], clone(expected)) {
							t.Fatalf("list differs from reference: %v / %v", rows, expected)
						}
						single := projectGET(f, "/api/projects/"+id)
						if single.Code != 200 {
							t.Fatal(single.Code)
						}
						var one Object
						if err := json.Unmarshal(single.Body.Bytes(), &one); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(rows[0], one) {
							t.Fatal("single/list differ", one, rows)
						}
						// Exercise every idempotent branch that returns present without changing
						// this state: create replay, chat replay, approval replay, create/file no-op.
						replies := []struct {
							path string
							body Object
							code int
						}{
							{"/api/projects", Object{"id": id, "text": "相談本文 <>&\n" + strings.Repeat("長い会話", 100)}, 202},
							{"/api/projects/" + id + "/chat", Object{"id": id, "text": "相談本文 <>&\n" + strings.Repeat("長い会話", 100)}, 202},
						}
						if valid && (status == "create_approved" || status == "creating" || status == "registered" || status == "creation_failed" || status == "creation_blocked") {
							replies = append(replies, struct {
								path string
								body Object
								code int
							}{"/api/projects/" + id + "/approve", Object{"fingerprint": Fingerprint(fleet.ObjectOrEmpty(p["definition"]))}, 200})
						}
						if status == "registered" || busy {
							replies = append(replies, struct {
								path string
								body Object
								code int
							}{"/api/projects/" + id + "/create", Object{}, 202})
						}
						if status == "registered" {
							replies = append(replies, struct {
								path string
								body Object
								code int
							}{"/api/projects/" + id + "/file", Object{}, 202})
						}
						for _, reply := range replies {
							code, got := f.call(reply.path, reply.body)
							if code != reply.code || !reflect.DeepEqual(got, one) {
								t.Fatalf("%s: %d %v, want %d %v", reply.path, code, got, reply.code, one)
							}
						}
						f.s.mu.Lock()
						after, err := f.s.load(context.Background(), id)
						f.s.mu.Unlock()
						if err != nil || !reflect.DeepEqual(before, after) {
							t.Fatal("presentation changed durable record", err, before, after)
						}
					})
				}
			}
		}
	}
}

func TestProjectListSnapshotWhileNumberResolutionWaits(t *testing.T) {
	f := newFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.options.ConfigFile = "" // approval persists without automatically creating
	f.options.DeskNumber = func(_ string, n int64) any {
		once.Do(func() { close(started); <-release })
		if n == 12 {
			return 5374
		}
		return nil
	}
	answerStarted, answerRelease := make(chan struct{}), make(chan struct{})
	f.options.Answer = func(ctx context.Context, _ Object) (Object, error) {
		close(answerStarted)
		select {
		case <-answerRelease:
			return answer(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.restart()
	// Ensure number waits are released even if an assertion fails.
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	a := presentationRecord(id, "ready")
	a["creation"], a["approval"] = nil, nil
	b := presentationRecord(otherProjectID, "ready")
	b["creation"], b["approval"], b["filing"] = nil, nil, nil
	savePresentationRecord(t, f, b, false)
	savePresentationRecord(t, f, a, true)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- projectGET(f, "/api/projects") }()
	projectReceive(t, started)
	if !f.s.mu.TryLock() {
		t.Fatal("list number resolution holds common mutex")
	}
	// A running task finishes while this list still owns its old record/flags.
	delete(f.s.running, id)
	f.s.mu.Unlock()
	// Approval is a real API mutation on the same project; remove filing from
	// its durable record so its response does not itself need number resolution.
	f.s.mu.Lock()
	current, err := f.s.load(context.Background(), id)
	if err == nil {
		current["filing"] = nil
		err = f.s.save(context.Background(), current)
	}
	f.s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	approvedDone := make(chan int, 1)
	go func() {
		code, _ := f.call("/api/projects/"+id+"/approve", Object{"fingerprint": Fingerprint(fleet.ObjectOrEmpty(a["definition"]))})
		approvedDone <- code
	}()
	if code := projectReceive(t, approvedDone); code != 200 {
		t.Fatal("approve during lookup", code)
	}
	chatDone := make(chan int, 1)
	go func() {
		code, _ := f.call("/api/projects/"+otherProjectID+"/chat", Object{"id": otherProjectID, "text": "次の相談"})
		chatDone <- code
	}()
	if code := projectReceive(t, chatDone); code != 202 {
		t.Fatal("unrelated chat during lookup", code)
	}
	projectReceive(t, answerStarted)
	// Configuration is captured too. Production Options are fixed at New, but
	// changing it under the lock here detects accidental rendering-time reads.
	f.s.mu.Lock()
	f.s.o.ConfigFile = "fixture"
	f.s.mu.Unlock()
	releaseOnce.Do(func() { close(release) })
	rows := decodeProjectList(t, projectReceive(t, done))
	if len(rows) != 2 || rows[0]["id"] != id || rows[1]["id"] != otherProjectID {
		t.Fatal(rows)
	}
	byID := map[string]Object{}
	for _, row := range rows {
		byID[fleet.Text(row["id"])] = row
	}
	for _, row := range rows {
		if row["status"] != "ready" || row["approvalValid"] != false || row["creationUnavailable"] != true || row["creationLocked"] != false {
			t.Fatal("mixed snapshot", row)
		}
	}
	if byID[id]["finishingTurn"] != true || byID[otherProjectID]["finishingTurn"] != false {
		t.Fatal("mixed running snapshot", rows)
	}
	if !reflect.DeepEqual(byID[id]["actions"], []any{"approve"}) || !reflect.DeepEqual(byID[otherProjectID]["actions"], []any{"chat", "approve"}) {
		t.Fatal("mixed actions", rows)
	}
	if len(fleet.List(fleet.ObjectOrEmpty(byID[id]["filing"])["issues"])) != 2 {
		t.Fatal("old snapshot lost fields")
	}
	// The ongoing chat exposes thinking/busy consistently to concurrent GETs.
	one := projectGET(f, "/api/projects/"+otherProjectID)
	if !strings.Contains(one.Body.String(), `"status":"thinking"`) {
		t.Fatal(one.Body.String())
	}
	close(answerRelease)
	if err := f.s.WaitAccepted(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows = decodeProjectList(t, projectGET(f, "/api/projects"))
	for _, row := range rows {
		if row["finishingTurn"] != false {
			t.Fatal("running did not finish", row)
		}
	}
}
