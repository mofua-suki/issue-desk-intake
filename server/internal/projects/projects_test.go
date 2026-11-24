package projects

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/gitea"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	t       *testing.T
	db      *store.SQLite
	s       *Service
	mux     *http.ServeMux
	options Options
	git     *fakeGit
}
type fakeGit struct {
	mu                           sync.Mutex
	repo                         Object
	branch                       Object
	createCalls, commits, grants int
	unknown                      bool
}

func (g *fakeGit) Do(_ context.Context, method, route string, data, out any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var value any
	switch {
	case route == "/user":
		value = Object{"login": "owner"}
	case route == "/user/repos" && method == "POST":
		g.createCalls++
		g.repo = Object{"id": float64(42), "default_branch": "main"}
		if g.unknown {
			g.unknown = false
			return &gitea.Error{Status: 502, UnknownOutcome: true, Cause: errors.New("lost response")}
		}
		value = g.repo
	case route == "/repos/owner/demo":
		if g.repo == nil {
			return &gitea.Error{Status: 404}
		}
		value = g.repo
	case route == "/repos/owner/demo/branches/main":
		if g.branch == nil {
			return &gitea.Error{Status: 404}
		}
		value = g.branch
	case route == "/repos/owner/demo/contents" && method == "POST":
		g.commits++
		g.branch = Object{"commit": Object{"id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
		value = Object{"commit": Object{"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	case route == "/users/intake":
		value = Object{"login": "intake"}
	case route == "/users/missing":
		return &gitea.Error{Status: 404}
	case route == "/repos/owner/demo/collaborators/intake" && method == "PUT":
		g.grants++
	default:
		return errors.New("unexpected fixture API: " + method + " " + route)
	}
	if out != nil {
		raw, _ := json.Marshal(value)
		return json.Unmarshal(raw, out)
	}
	return nil
}
func definition() Object {
	return Object{"name": "demo", "description": "利用者向けのプロジェクト\n説明", "technology": "node", "placement": "nas", "testCommand": []any{"{node}", "--test"}, "setupCommand": nil, "autoMerge": true, "autoDeploy": false, "splitIssues": []any{Object{"project": "owner/demo", "title": "最初の実装", "purpose": "アプリを動かす", "acceptance": []any{"動作を確認する"}, "dependsOn": []any{}, "changeScope": Object{"known": true, "paths": []any{"src/"}, "reason": "アプリ全体"}}}}
}
func answer() Object {
	d := definition()
	plans := d["splitIssues"]
	delete(d, "splitIssues")
	return Object{"ready": true, "reply": "定義を確認してください。", "questions": []any{}, "project": d, "splitIssues": plans}
}
func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := openStore(t, pgtest.NewIsolated(t))
	var e error
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if e = db.CreateSession(context.Background(), auth.Hash("screen"), auth.Session{CSRF: "csrf", ExpiresAt: now.Add(time.Hour)}, now); e != nil {
		t.Fatal(e)
	}
	a, e := auth.New(auth.Config{PublicURL: "http://127.0.0.1:8787", Now: func() time.Time { return now }}, &store.AuthStore{SQLite: db})
	if e != nil {
		t.Fatal(e)
	}
	f := &fixture{t: t, db: db, git: &fakeGit{}}
	f.options = Options{AppDB: db.DB(), Auth: a, Owner: "owner", Now: func() time.Time { return now }, Answer: func(context.Context, Object) (Object, error) { return answer(), nil }, Gitea: f.git, ConfigFile: "fixture", Preflight: func(context.Context, Object, Object) error { return nil }, Registration: func(context.Context, string, Object, Object) (bool, error) { return true, nil }, Registered: func(context.Context, Object) error { return nil }, Home: func(string) string { return "nas" }, Repositories: func() []string { return []string{} }, Collaborators: func(string) []string { return []string{"owner", "intake", "intake", "missing"} }}
	f.restart()
	t.Cleanup(func() { f.s.Close(); db.Close() })
	return f
}
func (f *fixture) restart() {
	f.t.Helper()
	if f.s != nil {
		f.s.Close()
	}
	f.mux = http.NewServeMux()
	var e error
	f.s, e = Register(f.mux, f.options)
	if e != nil {
		f.t.Fatal(e)
	}
}
func (f *fixture) call(path string, input any, headers ...map[string]string) (int, Object) {
	f.t.Helper()
	raw, _ := json.Marshal(input)
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787"+path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://127.0.0.1:8787")
	r.Header.Set("X-CSRF-Token", "csrf")
	r.AddCookie(&http.Cookie{Name: "desk_session", Value: "screen"})
	for _, h := range headers {
		for k, v := range h {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	out := Object{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

const id = "11111111-1111-4111-8111-111111111111"

func (f *fixture) wait(status string) Object {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// Read the durable state and running flag under the same lock. Otherwise
		// completion can occur between the two reads, returning an old snapshot.
		f.s.mu.Lock()
		p, e := f.s.load(context.Background(), id)
		busy := f.s.running[id]
		f.s.mu.Unlock()
		if e == nil && p["status"] == status && !busy {
			return p
		}
		time.Sleep(time.Millisecond)
	}
	p, _ := f.s.Snapshot(context.Background(), id)
	f.t.Fatalf("status did not become %s: %v", status, p)
	return nil
}
func TestProjectScreenApprovalDefinitionFenceAndCreate(t *testing.T) {
	f := newFixture(t)
	code, v := f.call("/api/projects", Object{"id": id, "text": "プロジェクトを作る"})
	if code != 202 {
		t.Fatalf("create %d %v", code, v)
	}
	p := f.wait("ready")
	if f.git.createCalls != 0 {
		t.Fatal("画面承認前に作成しました")
	}
	code, _ = f.call("/api/projects/"+id+"/approve", Object{"fingerprint": "bad"})
	if code != 409 {
		t.Fatal(code)
	}
	code, _ = f.call("/api/projects/"+id+"/approve", Object{"fingerprint": p["fingerprint"]}, map[string]string{"X-CSRF-Token": "bad"})
	if code != 403 {
		t.Fatal("CSRF bypass", code)
	}
	code, _ = f.call("/api/projects/"+id+"/approve", Object{"fingerprint": p["fingerprint"]}, map[string]string{"Origin": "https://attacker.invalid"})
	if code != 403 {
		t.Fatal("Origin bypass", code)
	}
	code, v = f.call("/api/projects/"+id+"/approve", Object{"fingerprint": p["fingerprint"]})
	if code != 200 {
		t.Fatalf("approve %d %v", code, v)
	}
	p = f.wait("registered")
	st := fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"])
	if f.git.createCalls != 1 || f.git.commits != 1 || f.git.grants != 1 || len(fleet.List(fleet.ObjectOrEmpty(st["collaborators"])["missing"])) != 1 {
		t.Fatalf("external steps not exact: %v", st)
	}
	code, _ = f.call("/api/projects/"+id+"/chat", Object{"text": "違う名前"})
	if code != 409 {
		t.Fatal("作成後の変更", code)
	}
}
func TestProjectUnknownCreateReconcilesAcrossRestart(t *testing.T) {
	f := newFixture(t)
	f.git.unknown = true
	_, _ = f.call("/api/projects", Object{"id": id, "text": "プロジェクト"})
	p := f.wait("ready")
	_, _ = f.call("/api/projects/"+id+"/approve", Object{"fingerprint": p["fingerprint"]})
	p = f.wait("creation_failed")
	if fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"])["requested"] == nil {
		t.Fatal("要求前の記録がありません")
	}
	f.restart()
	code, v := f.call("/api/projects/"+id+"/create", Object{})
	if code != 202 {
		t.Fatalf("retry %d %v", code, v)
	}
	f.wait("registered")
	if f.git.createCalls != 1 {
		t.Fatal("結果不明の作成を再送しました")
	}
}
func TestProjectSendIDAndAIRecovery(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	f.options.Answer = func(ctx context.Context, _ Object) (Object, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.restart()
	code, _ := f.call("/api/projects", Object{"id": id, "text": "一度の送信"})
	if code != 202 {
		t.Fatal(code)
	}
	<-started
	code, _ = f.call("/api/projects", Object{"id": id, "text": "一度の送信"})
	if code != 202 {
		t.Fatal(code)
	}
	code, _ = f.call("/api/projects", Object{"id": id, "text": "別の送信"})
	if code != 409 {
		t.Fatal(code)
	}
	f.s.Close()
	p, _ := f.s.Snapshot(context.Background(), id)
	if p["status"] != "thinking" {
		t.Fatal("停止時に復旧対象を失いました")
	}
	f.options.Answer = func(context.Context, Object) (Object, error) { return answer(), nil }
	f.restart()
	if e := f.s.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.wait("ready")
}
func TestDefinitionAndTemplates(t *testing.T) {
	d, e := validate(definition(), "owner", nil)
	if e != nil {
		t.Fatal(e)
	}
	files, e := ProjectFiles(d, "owner")
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, f := range files {
		if f.Path == ".gitignore" {
			found = true
		}
	}
	if !found {
		t.Fatal(".gitignore missing from embedded templates")
	}
	fingerprint := Fingerprint(d)
	if fingerprint != "746ef9d4b204b34d5f344f01a271c5244d968cf1474acb8f66c5022c093e2fb7" {
		t.Fatal("approval fingerprint incompatible", fingerprint)
	}
	d["autoMerge"] = false
	if Fingerprint(d) == fingerprint {
		t.Fatal("automation not in approval fingerprint")
	}
	d = definition()
	d["placement"] = "nas"
	d["technology"] = "python"
	if _, e = validate(d, "owner", nil); e == nil {
		t.Fatal("invalid NAS technology accepted")
	}
	d = definition()
	if _, e = validate(d, "owner", []string{"OWNER/DEMO"}); e == nil {
		t.Fatal("registered repository name accepted")
	}
	d = definition()
	fleet.List(d["splitIssues"])[0].(map[string]any)["dependsOn"] = []any{float64(1)}
	if _, e = validate(d, "owner", nil); e == nil {
		t.Fatal("cyclic dependency accepted")
	}
}
func TestPlacementPlanAndLocalRegistration(t *testing.T) {
	entry := registrationEntry(definition(), "owner")
	files := map[string]DeskFile{assignmentsFile: {Text: `{"hosts":{"nas":{"repositories":{}}}}`, SHA: "a"}, fleetFile: {Text: `{"hosts":{"nas":{"repositories":[]}}}`, SHA: "b"}}
	changes, e := PlanDeskChange(files, entry, "nas")
	if e != nil {
		t.Fatal(e)
	}
	if len(changes) != 3 {
		t.Fatal(changes)
	}
	for _, v := range changes {
		files[v.Path] = DeskFile{v.Text, v.SHA}
	}
	changes, e = PlanDeskChange(files, entry, "nas")
	if e != nil || len(changes) != 0 {
		t.Fatal("idempotent addition", e, changes)
	}
	files[assignmentsFile] = DeskFile{Text: `{"hosts":{"nas":{"repositories":{"owner/demo":{"rank":2}}}}}`, SHA: "a"}
	if _, e = PlanDeskChange(files, entry, "nas"); e == nil {
		t.Fatal("conflicting rank accepted")
	}
	directory := t.TempDir()
	configFile := filepath.Join(directory, "config.json")
	original := `{"repositories":[],"untouched":{"secretFile":"private/token"}}`
	if e = os.WriteFile(configFile, []byte(original), 0600); e != nil {
		t.Fatal(e)
	}
	r := NewRegistration(RegistrationOptions{ConfigFile: configFile, RuntimeDir: directory, ValidateConfig: func(Object) error { return nil }})
	if e = r.Registered(context.Background(), entry); e != nil {
		t.Fatal(e)
	}
	if e = r.Registered(context.Background(), entry); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(configFile)
	c, e := parseObject(string(raw))
	if e != nil || len(fleet.List(c["repositories"])) != 1 || fleet.ObjectOrEmpty(c["untouched"])["secretFile"] != "private/token" {
		t.Fatal("existing configuration changed", c)
	}
	backups, _ := filepath.Glob(filepath.Join(directory, "control", "config-before-register-*.json"))
	if len(backups) != 1 {
		t.Fatal("backup duplicated", backups)
	}
	b, _ := os.ReadFile(backups[0])
	if string(b) != original {
		t.Fatal("backup contents differ")
	}
}
func TestProjectFilingReceiptSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	ids := []string{}
	attempts := 0
	var registered atomic.Bool
	f.options.Repositories = func() []string {
		if registered.Load() {
			return []string{"owner/demo"}
		}
		return nil
	}
	f.options.Registered = func(context.Context, Object) error { registered.Store(true); return nil }
	f.options.Submit = func(_ context.Context, draft Object) ([]Object, error) {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, fleet.Text(draft["id"]))
		attempts++
		if draft["projectDefinition"] != id || draft["status"] != "auto_ready" || fleet.ObjectOrEmpty(draft["context"])["revision"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Error("filing context not pinned", draft)
		}
		if attempts == 1 {
			return nil, errors.New("unknown filing result")
		}
		return []Object{{"project": "owner/demo", "number": float64(12), "url": "https://gitea.invalid/owner/demo/issues/12"}}, nil
	}
	f.restart()
	_, _ = f.call("/api/projects", Object{"id": id, "text": "作成"})
	p := f.wait("ready")
	_, _ = f.call("/api/projects/"+id+"/approve", Object{"fingerprint": p["fingerprint"]})
	p = f.wait("registered")
	if fleet.ObjectOrEmpty(p["filing"])["status"] != "failed" {
		t.Fatal("failure not persisted", p)
	}
	f.restart()
	if e := f.s.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
	p = f.wait("registered")
	if fleet.ObjectOrEmpty(p["filing"])["status"] != "filed" || len(ids) != 2 || ids[0] != ids[1] {
		t.Fatal("filing identity changed", p, ids)
	}
}
func TestProjectNASPlacementWaitResumes(t *testing.T) {
	f := newFixture(t)
	ready := false
	var mu sync.Mutex
	f.options.Registration = func(context.Context, string, Object, Object) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return ready, nil
	}
	f.restart()
	_, _ = f.call("/api/projects", Object{"id": id, "text": "作成"})
	p := f.wait("ready")
	_, _ = f.call("/api/projects/"+id+"/approve", Object{"fingerprint": p["fingerprint"]})
	p = f.wait("creating")
	if fleet.ObjectOrEmpty(fleet.ObjectOrEmpty(p["creation"])["steps"])["registered"] != nil {
		t.Fatal("placement wait bypassed")
	}
	mu.Lock()
	ready = true
	mu.Unlock()
	f.restart()
	if e := f.s.Recover(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.wait("registered")
	if f.git.createCalls != 1 || f.git.commits != 1 {
		t.Fatal("recovery repeated external writes")
	}
}

type apiFunc func(context.Context, string, string, any, any) error

func (f apiFunc) Do(ctx context.Context, m, p string, d, o any) error { return f(ctx, m, p, d, o) }
func TestPlacementPRDiffFence(t *testing.T) {
	entry := registrationEntry(definition(), "owner")
	base := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	head := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	baseFiles := map[string]DeskFile{assignmentsFile: {Text: `{"hosts":{"nas":{"repositories":{}}}}`, SHA: "1"}, fleetFile: {Text: `{"hosts":{"nas":{"repositories":[]}}}`, SHA: "2"}}
	changes, e := PlanDeskChange(baseFiles, entry, "nas")
	if e != nil {
		t.Fatal(e)
	}
	headFiles := map[string]DeskFile{}
	for _, c := range changes {
		headFiles[c.Path] = DeskFile{c.Text, "head"}
	}
	tamper := false
	extra := false
	rename := false
	r := NewRegistration(RegistrationOptions{Repo: "owner/desk", Gitea: apiFunc(func(_ context.Context, m, route string, _ any, out any) error {
		var value any
		if strings.Contains(route, "/pulls/1/files") {
			files := []Object{}
			for _, c := range changes {
				status := "changed"
				if c.SHA == "" {
					status = "added"
				}
				f := Object{"filename": c.Path, "status": status}
				if rename {
					f["previous_filename"] = "private/settings"
				}
				files = append(files, f)
			}
			if extra {
				files = append(files, Object{"filename": "AGENTS.md", "status": "changed"})
			}
			value = files
		} else {
			parts := strings.SplitN(route, "/contents/", 2)
			if len(parts) != 2 {
				return errors.New("unexpected fixture route")
			}
			parts = strings.SplitN(parts[1], "?ref=", 2)
			file, ref := parts[0], parts[1]
			source := baseFiles
			if ref == head {
				source = headFiles
			}
			f, ok := source[file]
			if !ok {
				return &gitea.Error{Status: 404}
			}
			text := f.Text
			if tamper && ref == head && file == assignmentsFile {
				text += " "
			}
			value = Object{"content": base64.StdEncoding.EncodeToString([]byte(text)), "sha": f.SHA}
		}
		raw, _ := json.Marshal(value)
		return json.Unmarshal(raw, out)
	})})
	pr := Object{"number": float64(1), "merge_base": base, "head": Object{"sha": head}}
	if e = r.verify(context.Background(), pr, definition(), entry); e != nil {
		t.Fatal(e)
	}
	tamper = true
	if e = r.verify(context.Background(), pr, definition(), entry); e == nil {
		t.Fatal("head text change accepted")
	}
	tamper = false
	extra = true
	if e = r.verify(context.Background(), pr, definition(), entry); e == nil {
		t.Fatal("extra file accepted")
	}
	extra = false
	rename = true
	if e = r.verify(context.Background(), pr, definition(), entry); e == nil {
		t.Fatal("rename accepted")
	}
}

func TestProjectFivePathPlanIsOutsideConsultationSizeCheck(t *testing.T) {
	f := newFixture(t)
	f.options.Answer = func(context.Context, Object) (Object, error) {
		v := answer()
		plan := fleet.List(v["splitIssues"])[0].(Object)
		plan["changeScope"] = Object{"known": true, "paths": []any{"a.go", "b.go", "c.go", "d.go", "e.go"}, "reason": ""}
		return v, nil
	}
	f.restart()
	code, v := f.call("/api/projects", Object{"id": id, "text": "プロジェクトを作る"})
	if code != 202 {
		t.Fatalf("create %d %v", code, v)
	}
	p := f.wait("ready")
	plan := fleet.ObjectOrEmpty(fleet.List(fleet.ObjectOrEmpty(p["definition"])["splitIssues"])[0])
	if paths := fleet.List(fleet.ObjectOrEmpty(plan["changeScope"])["paths"]); len(paths) != 5 {
		t.Fatalf("paths=%v", paths)
	}
}

func TestNewRequiresAuth(t *testing.T) {
	if _, err := New(Options{AppDB: &sql.DB{}}); err == nil {
		t.Fatal("New accepted AppDB without Auth")
	}
}
