package httpapi

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hosts"
)

func TestHostHeartbeatPreservesAllFieldsPostgres(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	handler, db, token := workerFixtureOptions(t, false, func(o *WorkerRouteOptions) { o.Now = func() time.Time { return at } })
	stamp := at.Format(time.RFC3339Nano)
	body := map[string]any{
		"revision": "1111111111111111111111111111111111111111", "imageRevision": "2222222222222222222222222222222222222222",
		"cliVersions": []any{map[string]any{"cli": "codex", "current": "0.160.1", "expected": "0.160.1", "at": at.UnixMilli()}},
		"load":        map[string]any{"cpuPercent": 0, "freeMemoryMB": 12.5, "freeDiskMB": 30.75, "gpuPercent": 0, "usedVramMB": 0, "freeVramMB": 1.5, "loadAverage1m": 0, "logicalCpuCount": 2},
		"busy":        true, "implementing": 1, "publishing": 2,
		"capabilities": map[string]any{"platform": "linux", "providers": []any{"codex", "claude"}, "repositories": []any{"owner/repo"}, "workerTokenKind": "worker", "antigravity": false},
		"ready":        true, "checks": map[string]any{"at": stamp, "ok": true, "items": []any{map[string]any{"name": "fixture", "label": "検査", "ok": true, "detail": "確認済み"}}, "settingsVersion": "17"},
		"capacity": 3, "intakeHold": map[string]any{"reasons": []any{map[string]any{"kind": "usage_limit", "since": stamp, "until": at.Add(time.Hour).Format(time.RFC3339Nano), "detail": "上限待ち", "provider": "codex", "source": "fixture", "limitKind": "five_hour"}}},
	}
	raw, _ := json.Marshal(body)
	status, result := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", string(raw))
	if status != 200 {
		t.Fatal(status, result)
	}
	var saved []byte
	if e := db.QueryRow(`SELECT jsonb_build_object('revision',revision,'imageRevision',image_revision,'cliVersions',cli_versions,'load',load,'busy',busy,'implementing',implementing,'publishing',publishing,'capabilities',capabilities,'ready',ready,'checks',checks,'capacity',capacity,'intakeHold',intake_hold)FROM hosts WHERE host='nas'`).Scan(&saved); e != nil {
		t.Fatal(e)
	}
	var want, got map[string]any
	if e := json.Unmarshal(raw, &want); e != nil {
		t.Fatal(e)
	}
	if e := json.Unmarshal(saved, &got); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("heartbeatの全項目または単位が保存結果と異なります", want, got)
	}
	var sample []byte
	if e := db.QueryRow("SELECT load FROM host_load_samples WHERE host='nas'ORDER BY received_at DESC LIMIT 1").Scan(&sample); e != nil {
		t.Fatal(e)
	}
	var load map[string]any
	if e := json.Unmarshal(sample, &load); e != nil || !reflect.DeepEqual(load, want["load"]) {
		t.Fatal("負荷履歴の0/欠測/MB単位が変わりました", load, e)
	}
}

func TestPublicOverloadKeepsMissingHeartbeatSamplesPostgres(t *testing.T) {
	_, db, _ := workerFixture(t)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	now := at.Add(-2 * time.Minute)
	service := &hosts.Service{DB: db, Now: func() time.Time { return now }}
	high := map[string]any{"loadAverage1m": 4.0, "logicalCpuCount": 2.0}
	for i, load := range []any{high, nil, high} {
		now = at.Add(time.Duration(i-2) * time.Minute)
		if _, e := service.Put(context.Background(), "sample-host", map[string]any{"load": load}); e != nil {
			t.Fatal(e)
		}
	}
	limit := map[string]any{"ratio": 1.0, "cpuPercent": 90.0, "minutes": 10.0}
	assignments := &fleet.Assignments{Output: map[string]any{"dispatch": map[string]any{"loadLimit": limit, "leaseMinutes": 15.0}}}
	view, e := service.View(context.Background(), assignments)
	if e != nil {
		t.Fatal(e)
	}
	for _, raw := range view.(map[string]any)["hosts"].([]any) {
		row := raw.(map[string]any)
		if row["name"] == "sample-host" {
			if row["overloaded"] != false {
				t.Fatal("欠測を捨て高負荷と誤判定しました", row)
			}
			history := row["loadHistory"].(map[string]any)
			if history["cpuPercent"] != nil {
				t.Fatal("未測定CPUを0と扱いました", history)
			}
			if history["loadAverage1m"].(map[string]any)["count"] != 2 {
				t.Fatal("有効標本の件数が変わりました", history)
			}
			return
		}
	}
	t.Fatal("検証hostが一覧にありません")
}

func TestHeartbeatRetainsExactThreeHourHistoryWindowPostgres(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	handler, db, token := workerFixtureOptions(t, false, func(o *WorkerRouteOptions) { o.Now = func() time.Time { return at } })
	if status, result := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{}`); status != 200 {
		t.Fatal(status, result)
	}
	cutoff := at.Add(-3 * time.Hour)
	for _, sample := range []struct {
		host string
		at   time.Time
	}{
		{"nas", cutoff.Add(-time.Millisecond)}, {"other", cutoff.Add(-time.Millisecond)},
		{"nas", cutoff}, {"other", at.Add(-time.Hour)}, {"future", at.Add(time.Hour)},
	} {
		if _, e := db.Exec("INSERT INTO host_load_samples(host,received_at,load)VALUES($1,$2,$3::jsonb)", sample.host, sample.at, `{"cpuPercent":0}`); e != nil {
			t.Fatal(e)
		}
	}
	if status, result := workerRequest(t, handler, token, "PUT", "/api/v1/hosts/nas", `{"load":null}`); status != 200 {
		t.Fatal(status, result)
	}
	var expired, remaining int
	if e := db.QueryRow("SELECT count(*)FILTER(WHERE received_at<$1),count(*)FROM host_load_samples", cutoff).Scan(&expired, &remaining); e != nil || expired != 2 || remaining != 7 {
		t.Fatal("通知元の期限外だけを整理できません", expired, remaining, e)
	}
	for _, sample := range []struct {
		host string
		at   time.Time
	}{{"nas", cutoff}, {"other", at.Add(-time.Hour)}, {"future", at.Add(time.Hour)}} {
		var count int
		if e := db.QueryRow("SELECT count(*)FROM host_load_samples WHERE host=$1 AND received_at=$2", sample.host, sample.at).Scan(&count); e != nil || count != 1 {
			t.Fatal("境界または他の標本を失いました", sample, count, e)
		}
	}
	service := &hosts.Service{DB: db, Now: func() time.Time { return at }}
	assignments := &fleet.Assignments{Output: map[string]any{"dispatch": map[string]any{"loadLimit": map[string]any{"ratio": 1.0, "cpuPercent": 90.0, "minutes": 180.0}, "leaseMinutes": 15.0}}}
	view, e := service.View(context.Background(), assignments)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, raw := range view.(map[string]any)["hosts"].([]any) {
		row := raw.(map[string]any)
		if row["name"] == "nas" {
			found = true
			history := row["loadHistory"].(map[string]any)
			if history["cpuPercent"].(map[string]any)["count"] != 1 || row["overloaded"] != false {
				t.Fatal("期限外標本が表示または判定に混入しました", row)
			}
		}
	}
	if !found {
		t.Fatal("nas が一覧にありません")
	}
	var missing int
	if e := db.QueryRow("SELECT count(*)FROM host_load_samples WHERE host='nas'AND received_at=$1 AND load IS NULL", at).Scan(&missing); e != nil || missing != 2 {
		t.Fatal("受信した欠測標本を保存できません", missing, e)
	}
}
