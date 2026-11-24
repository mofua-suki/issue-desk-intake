package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hosts"
	"issue-desk-intake/server/internal/screenapi"
)

func TestHostRepositoriesRoutesAndAssignmentChangesPostgres(t *testing.T) {
	var options *WorkerRouteOptions
	handler, db, token := workerFixtureOptions(t, false, func(o *WorkerRouteOptions) { options = o })
	write := func(repos string) {
		t.Helper()
		data := `{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":2,"repositories":` + repos + `},"empty":{"maxImplementations":1}},"repositories":{}}`
		if err := os.WriteFile(options.AssignmentsPath, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"owner/z":{"rank":2},"owner/b":{"rank":1},"owner/a":{"rank":1}}`)
	if _, err := db.Exec(`INSERT INTO hosts(host,heartbeat_at) VALUES ('record-only',now())`); err != nil {
		t.Fatal(err)
	}
	check := func(view map[string]any, want []any) {
		t.Helper()
		rows := map[string]map[string]any{}
		for _, raw := range view["hosts"].([]any) {
			row := raw.(map[string]any)
			rows[row["name"].(string)] = row
		}
		for name, expected := range map[string]any{"nas": want, "empty": []any{}, "record-only": nil} {
			value, ok := rows[name]["repositories"]
			if !ok || !reflect.DeepEqual(value, expected) {
				t.Fatalf("%s repositories=%#v, want %#v", name, value, expected)
			}
		}
		for name, expected := range map[string]any{"empty": []any{}, "record-only": nil} {
			if !reflect.DeepEqual(rows[name]["repositoryLoads"], expected) {
				t.Fatalf("%s loads=%#v", name, rows[name]["repositoryLoads"])
			}
		}
		loads := rows["nas"]["repositoryLoads"].([]any)
		if len(loads) != len(want) {
			t.Fatalf("loads=%#v", loads)
		}
		for i, raw := range loads {
			load := raw.(map[string]any)
			if load["repository"] != want[i] || load["max"] != float64(2) || load["implementing"] != float64(0) || load["publishing"] != float64(0) {
				t.Fatalf("load=%#v", load)
			}
		}

	}
	get := func() map[string]any {
		t.Helper()
		status, view := workerRequest(t, handler, token, "GET", "/api/hosts", "")
		if status != 200 {
			t.Fatal(status, view)
		}
		return view
	}
	check(get(), []any{"owner/a", "owner/b", "owner/z"})
	// The list path uses the same View and its delta serialization, including updates.
	load := func(context.Context) (screenapi.Snapshot, error) {
		raw, err := os.ReadFile(options.AssignmentsPath)
		if err != nil {
			return screenapi.Snapshot{}, err
		}
		var data map[string]any
		if err = json.Unmarshal(raw, &data); err != nil {
			return screenapi.Snapshot{}, err
		}
		a, err := fleet.NormalizeAssignments(data)
		if err != nil {
			return screenapi.Snapshot{}, err
		}
		view, err := (&hosts.Service{DB: db, Now: time.Now}).View(context.Background(), a)
		return screenapi.Snapshot{Hosts: view}, err
	}
	delta := screenapi.NewDelta(screenapi.DeltaOptions{})
	first, err := delta.Respond(context.Background(), "screen", nil, load)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(first["hosts"])
	var listView map[string]any
	if err = json.Unmarshal(encoded, &listView); err != nil {
		t.Fatal(err)
	}
	check(listView, []any{"owner/a", "owner/b", "owner/z"})
	cursor := first["cursor"].(string)
	write(`{"owner/new":{"rank":1}}`)
	check(get(), []any{"owner/new"})
	next, err := delta.Respond(context.Background(), "screen", &cursor, load)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(next["hosts"])
	if err = json.Unmarshal(encoded, &listView); err != nil {
		t.Fatal(err)
	}
	check(listView, []any{"owner/new"})
}

func TestHostRepositoriesShippedAssignmentsPostgres(t *testing.T) {
	_, db, _ := workerFixture(t)
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
	view, err := (&hosts.Service{DB: db, Now: time.Now}).View(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]any{}
	for _, item := range view.(map[string]any)["hosts"].([]any) {
		row := item.(map[string]any)
		rows[row["name"].(string)] = row
	}
	retired := map[string]bool{}
	for _, name := range fleet.Strings(a.Output["retiredHosts"]) {
		retired[name] = true
	}
	for name, rawHost := range data["hosts"].(map[string]any) {
		if retired[name] {
			continue
		}
		expected := map[string]bool{}
		for repo := range fleet.Object(fleet.Object(rawHost)["repositories"]) {
			expected[repo] = true
		}
		got, ok := rows[name]["repositories"].([]string)
		if !ok {
			t.Fatalf("%s: missing repositories", name)
		}
		loads, ok := rows[name]["repositoryLoads"].([]any)
		if !ok || len(loads) != len(got) {
			t.Fatalf("%s: loads=%#v", name, rows[name]["repositoryLoads"])
		}
		for i, repo := range got {
			load := loads[i].(map[string]any)
			if load["repository"] != repo || load["max"] != a.RepoLimit(name, repo) {
				t.Fatalf("%s: load=%#v", name, load)
			}
		}
		for _, repo := range got {
			if !expected[repo] {
				t.Fatalf("%s: unexpected %s", name, repo)
			}
			delete(expected, repo)
		}
		if len(expected) != 0 {
			t.Fatalf("%s: missing %v", name, expected)
		}
	}
}

func TestHostRepositoryLoadsPostgres(t *testing.T) {
	var options *WorkerRouteOptions
	handler, db, token := workerFixtureOptions(t, false, func(o *WorkerRouteOptions) { options = o })
	data := `{"pollSeconds":300,"hosts":{"nas":{"maxImplementations":3,"repositories":{"owner/repo":{"rank":1,"max":2}}},"empty":{"maxImplementations":1}},"repositories":{}}`
	if err := os.WriteFile(options.AssignmentsPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO hosts(host,heartbeat_at) VALUES ('record-only',now())`); err != nil {
		t.Fatal(err)
	}
	// The current clock is held fixed for the exact expiry boundary.
	at := time.Now().UTC().Truncate(time.Microsecond)
	for _, item := range []struct {
		issue        int
		host, status string
		expiry       time.Time
		view         string
	}{
		{1, "nas", "running", at.Add(time.Hour), `{}`}, {2, "nas", "running", at.Add(time.Hour), `{"phase":"implementing"}`},
		{3, "nas", "running", at.Add(time.Hour), `{"phase":"publishing"}`},
		{5, "nas", "running", at, `{}`}, {6, "nas", "running", at.Add(-time.Second), `{}`}, {7, "other", "running", at.Add(time.Hour), `{}`}, {8, "nas", "queued", at.Add(time.Hour), `{}`},
	} {
		if _, err := db.Exec(`INSERT INTO jobs(repo,issue,status,lease_host,lease_expires_at,view) VALUES('owner/repo',$1,$2,$3,$4,$5::jsonb)`, item.issue, item.status, item.host, item.expiry, item.view); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO jobs(repo,issue,status,lease_host,lease_expires_at,view) VALUES ('other/repo', 9, 'running', 'nas', $1, '{"phase":"publishing"}')`, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(options.AssignmentsPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err = json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	a, err := fleet.NormalizeAssignments(config)
	if err != nil {
		t.Fatal(err)
	}
	service := &hosts.Service{DB: db, Now: func() time.Time { return at }}
	check := func(view map[string]any, implementing, publishing, totalImplementing, totalPublishing any) {
		t.Helper()
		rows := map[string]map[string]any{}
		for _, raw := range view["hosts"].([]any) {
			row := raw.(map[string]any)
			rows[row["name"].(string)] = row
		}
		if totalImplementing == nil {
			if rows["nas"]["hostLoad"] != nil {
				t.Fatalf("hostLoad=%#v", rows["nas"]["hostLoad"])
			}
		} else {
			encoded, _ := json.Marshal(rows["nas"]["hostLoad"])
			var total map[string]any
			if err := json.Unmarshal(encoded, &total); err != nil {
				t.Fatal(err)
			}
			if total["implementing"] != totalImplementing || total["publishing"] != totalPublishing {
				t.Fatalf("hostLoad=%#v", total)
			}
		}
		loads := rows["nas"]["repositoryLoads"].([]any)
		row := loads[0].(map[string]any)
		if row["repository"] != "owner/repo" || floatNumber(row["max"]) != float64(a.RepoLimit("nas", "owner/repo")) || floatNumber(row["implementing"]) != floatNumber(implementing) || floatNumber(row["publishing"]) != floatNumber(publishing) {
			t.Fatalf("loads=%#v", row)
		}
		if !reflect.DeepEqual(rows["empty"]["repositoryLoads"], []any{}) || rows["record-only"]["repositoryLoads"] != nil {
			t.Fatalf("empty/unknown: %#v", rows)
		}
	}
	view, err := service.View(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	check(view.(map[string]any), 2, 1, float64(2), float64(2))
	delta := screenapi.NewDelta(screenapi.DeltaOptions{})
	load := func(ctx context.Context) (screenapi.Snapshot, error) {
		v, e := service.View(ctx, a)
		return screenapi.Snapshot{Hosts: v}, e
	}
	first, err := delta.Respond(context.Background(), "screen", nil, load)
	if err != nil {
		t.Fatal(err)
	}
	cursor := first["cursor"].(string)
	if _, err := db.Exec(`INSERT INTO jobs(repo,issue,status,lease_host,lease_expires_at,view) VALUES('owner/repo',4,'running','nas',$1,'{"releasePending":true}')`, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	view, err = service.View(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	check(view.(map[string]any), 2, 2, float64(2), float64(3))
	status, response := workerRequest(t, handler, token, "GET", "/api/hosts", "")
	if status != 200 || response["hosts"] == nil {
		t.Fatal(status, response)
	}
	check(response, float64(2), float64(2), float64(2), float64(3))
	next, err := delta.Respond(context.Background(), "screen", &cursor, load)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(next["hosts"])
	if err != nil {
		t.Fatal(err)
	}
	var listView map[string]any
	if err = json.Unmarshal(encoded, &listView); err != nil {
		t.Fatal(err)
	}
	check(listView, float64(2), float64(2), float64(2), float64(3))
	if _, err = db.Exec(`ALTER TABLE jobs RENAME TO jobs_unavailable`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`ALTER TABLE jobs_unavailable RENAME TO jobs`) })
	view, err = service.View(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	check(view.(map[string]any), nil, nil, nil, nil)
	status, response = workerRequest(t, handler, token, "GET", "/api/hosts", "")
	if status != 200 || response["hosts"] == nil {
		t.Fatal(status, response)
	}
	check(response, nil, nil, nil, nil)
	cursor = next["cursor"].(string)
	failed, err := delta.Respond(context.Background(), "screen", &cursor, load)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(failed["hosts"])
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &listView); err != nil {
		t.Fatal(err)
	}
	check(listView, nil, nil, nil, nil)
}

func floatNumber(v any) any {
	switch n := v.(type) {
	case int:
		return float64(n)
	default:
		return v
	}
}
