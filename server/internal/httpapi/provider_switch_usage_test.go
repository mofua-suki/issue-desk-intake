package httpapi

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// 使用量の入口は担当job/providerの固定ではなく、実行したproviderの測定行を累計する。
func TestClaudeUsageAdmissionAfterAntigravitySwitchKeepsFenceAndBudget(t *testing.T) {
	handler, db, token := workerFixture(t)
	var id int64
	if e := db.QueryRow(`INSERT INTO jobs(repo,issue,title,provider,status,lease_host,lease_epoch,lease_expires_at,view)VALUES('owner/repo',17,'switch','antigravity','running','nas',1,now()+interval '1 hour','{"providerSwitch":{"from":"antigravity","to":"claude"},"antigravityStrikes":2}')RETURNING id`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	path := "/api/v1/jobs/" + fmtInt(id) + "/events"
	row := map[string]any{"at": "2026-11-01T00:00:00.000Z", "host": "nas", "source": "worker", "repo": "owner/repo", "issue": 17, "provider": "claude", "step": "実装", "inputTokens": 100, "cacheReadTokens": 0, "cacheWriteTokens": 0, "outputTokens": 0}
	body := func(epoch int) string {
		raw, _ := json.Marshal(map[string]any{"epoch": epoch, "kind": "implementation_usage_check", "data": map[string]any{"rows": []any{row}, "skipped": 0}})
		return string(raw)
	}
	status, result := workerRequest(t, handler, token, "POST", path, body(1))
	if status != 200 || result["usageLimitReason"] != nil {
		t.Fatal("AGYから切替後のClaude測定を拒否しました", status, result)
	}
	job := result["job"].(map[string]any)
	if job["provider"] != "antigravity" || job["view"].(map[string]any)["antigravityStrikes"] != float64(2) {
		t.Fatal("元のprovider/切替証跡を変えました", job)
	}
	delete(row, "cacheWriteTokens")
	status, result = workerRequest(t, handler, token, "POST", path, body(1))
	if status != 200 || result["usageLimitReason"] != nil {
		t.Fatal("初回の補完できない欠測を拒否しました", status, result)
	}
	// 2回目以降は欠測を未知として扱う契約を維持する。
	if _, e := db.Exec(`UPDATE jobs SET generation=1 WHERE id=$1`, id); e != nil {
		t.Fatal(e)
	}
	status, result = workerRequest(t, handler, token, "POST", path, body(1))
	if status != 200 || result["usageLimitReason"] == nil {
		t.Fatal("切替でClaudeの未測定使用量を既知扱いしました", status, result)
	}
	row["cacheWriteTokens"] = 0
	row["inputTokens"] = 9000000
	status, result = workerRequest(t, handler, token, "POST", path, body(1))
	if status != 200 || result["usageLimitReason"] == nil {
		t.Fatal("切替後の使用量上限を迂回しました", status, result)
	}
	if status, result = workerRequest(t, handler, token, "POST", path, body(2)); status != 409 || result["code"] != "lease_lost" {
		t.Fatal("切替でepoch境界を迂回しました", status, result)
	}
}

func TestUsageAdmissionPersistsEstimatesAndCountsExcludedGenerations(t *testing.T) {
	for _, key := range []string{"retestPassedGenerations", "prerequisiteMissingGenerations", "codexNotStartedGenerations"} {
		t.Run(key, func(t *testing.T) {
			handler, db, token := workerFixture(t)
			var id int64
			if err := db.QueryRow(`INSERT INTO jobs(repo,issue,title,status,generation,lease_host,lease_epoch,lease_expires_at,view) VALUES('owner/repo',18,'boundary','running',1,'nas',1,now()+interval '1 hour','{}') RETURNING id`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			row := map[string]any{"at": "2026-11-01T00:00:00.000Z", "host": "nas", "source": "worker", "repo": "owner/repo", "issue": 18, "provider": "claude", "step": "実装", "inputTokens": 100, "cacheReadTokens": 0, "outputTokens": 0}
			body, _ := json.Marshal(map[string]any{"epoch": 1, "kind": "implementation_usage_check", "data": map[string]any{"rows": []any{row}, "skipped": 0}})
			check := func() (int, map[string]any) {
				return workerRequest(t, handler, token, "POST", "/api/v1/jobs/"+fmtInt(id)+"/events", string(body))
			}
			view, _ := json.Marshal(map[string]any{key: []int{1}})
			if _, err := db.Exec(`UPDATE jobs SET view=$2::jsonb WHERE id=$1`, id, string(view)); err != nil {
				t.Fatal(err)
			}
			status, result := check()
			if status != 200 || result["usageLimitReason"] != nil {
				t.Fatalf("excluded: %d %v", status, result)
			}
			status, detail := workerRequest(t, handler, token, "GET", "/api/v1/jobs/"+fmtInt(id), "")
			if status != 200 {
				t.Fatalf("job detail: %d %v", status, detail)
			}
			var event map[string]any
			for _, raw := range detail["events"].([]any) {
				item := raw.(map[string]any)
				if item["kind"] == "implementation_usage_estimate" {
					event = item["data"].(map[string]any)
				}
			}
			if event == nil {
				t.Fatalf("estimate event missing: %v", detail)
			}
			estimate := event["estimate"].(map[string]any)
			steps := estimate["steps"].([]any)
			if event["epoch"] != float64(1) || estimate["missingRows"] != float64(1) || estimate["total"] != nil || len(steps) != 1 || steps[0].(map[string]any)["step"] != "実装" || steps[0].(map[string]any)["missingRows"] != float64(1) || steps[0].(map[string]any)["estimated"] != nil || steps[0].(map[string]any)["fallbackRows"] != float64(0) {
				t.Fatalf("event: %v", event)
			}
			if _, err := db.Exec(`UPDATE jobs SET view='{}'::jsonb WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			status, result = check()
			if status != 200 || result["usageLimitReason"] == nil {
				t.Fatalf("without exclusion: %d %v", status, result)
			}
			var estimates, limits int
			if err := db.QueryRow(`SELECT count(*) FILTER (WHERE kind='implementation_usage_estimate'), count(*) FILTER (WHERE kind='implementation_usage_limit') FROM job_events WHERE job_id=$1`, id).Scan(&estimates, &limits); err != nil || estimates != 1 || limits != 1 {
				t.Fatalf("events: estimates=%d limits=%d: %v", estimates, limits, err)
			}
		})
	}
}

// 状態報告で保存された除外記録を、再取得後の使用量判定が読み取る。
// retest/codexの2キーは旧Workerの保存値との互換用。現Workerが新規生成するpayloadとは扱わない。
func TestUsageAdmissionAfterExclusionStateReport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report string
		key    string
	}{
		{"retest passed", `{"epoch":1,"status":"running","view":{"retestPassedGenerations":[1]}}`, "retestPassedGenerations"},
		{"prerequisite missing", `{"epoch":1,"status":"held","holdReason":"prerequisite_missing","result":{"status":"failed","questions":[],"prerequisiteMissing":{"reason":"missing","artifacts":[{"artifact":"API","repo":"owner/repo","issue":1}]}}}`, "prerequisiteMissingGenerations"},
		{"codex not started", `{"epoch":1,"status":"running","view":{"codexNotStartedGenerations":[1]}}`, "codexNotStartedGenerations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, db, token := workerFixture(t)
			var id int64
			if err := db.QueryRow(`INSERT INTO jobs(repo,issue,title,status,generation,lease_host,lease_epoch,lease_expires_at,view) VALUES('owner/repo',18,'boundary','running',1,'nas',1,now()+interval '1 hour','{}') RETURNING id`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			base := "/api/v1/jobs/" + fmtInt(id)
			status, result := workerRequest(t, handler, token, "POST", base+"/state", tc.report)
			if status != 200 {
				t.Fatalf("state: %d %v", status, result)
			}
			status, detail := workerRequest(t, handler, token, "GET", base, "")
			if status != 200 {
				t.Fatalf("detail: %d %v", status, detail)
			}
			view := detail["job"].(map[string]any)["view"].(map[string]any)
			values, ok := view[tc.key].([]any)
			if !ok || len(values) != 1 || values[0] != float64(1) {
				t.Fatalf("stored view: %v", view)
			}
			// 前提待ちは lease を解放するため、次の実行の lease だけを用意する。
			if tc.key == "prerequisiteMissingGenerations" {
				if _, err := db.Exec(`UPDATE jobs SET status='running',hold_reason=NULL,lease_host='nas',lease_epoch=2,lease_expires_at=now()+interval '1 hour' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			epoch := 1
			if tc.key == "prerequisiteMissingGenerations" {
				epoch = 2
			}
			row := map[string]any{"at": "2026-11-01T00:00:00.000Z", "host": "nas", "source": "worker", "repo": "owner/repo", "issue": 18, "provider": "claude", "step": "実装", "inputTokens": 100, "cacheReadTokens": 0, "outputTokens": 0}
			body, err := json.Marshal(map[string]any{"epoch": epoch, "kind": "implementation_usage_check", "data": map[string]any{"rows": []any{row}, "skipped": 0}})
			if err != nil {
				t.Fatal(err)
			}
			status, result = workerRequest(t, handler, token, "POST", base+"/events", string(body))
			if status != 200 || result["usageLimitReason"] != nil {
				t.Fatalf("excluded usage: %d %v", status, result)
			}
			// 前提待ちの履歴は以後の状態報告でも保持される。除外がない実行は別jobで比較する。
			var controlID int64
			if err := db.QueryRow(`INSERT INTO jobs(repo,issue,title,status,generation,lease_host,lease_epoch,lease_expires_at,view) VALUES('owner/repo',19,'without exclusion','running',1,'nas',1,now()+interval '1 hour','{}') RETURNING id`).Scan(&controlID); err != nil {
				t.Fatal(err)
			}
			controlBase := "/api/v1/jobs/" + fmtInt(controlID)
			status, result = workerRequest(t, handler, token, "POST", controlBase+"/state", `{"epoch":1,"status":"running","view":{}}`)
			if status != 200 {
				t.Fatalf("state without exclusion: %d %v", status, result)
			}
			status, detail = workerRequest(t, handler, token, "GET", controlBase, "")
			if status != 200 || detail["job"].(map[string]any)["view"].(map[string]any)[tc.key] != nil {
				t.Fatalf("unexpected exclusion in control: %d %v", status, detail)
			}
			row["issue"] = 19
			controlBody, err := json.Marshal(map[string]any{"epoch": 1, "kind": "implementation_usage_check", "data": map[string]any{"rows": []any{row}, "skipped": 0}})
			if err != nil {
				t.Fatal(err)
			}
			status, result = workerRequest(t, handler, token, "POST", controlBase+"/events", string(controlBody))
			if status != 200 || result["usageLimitReason"] == nil {
				t.Fatalf("missing usage without exclusion did not stop: %d %v", status, result)
			}
		})
	}
}

// 実測と欠測を受け付けた判定の内訳がイベント保存・再読込を経ても保たれる。
func TestUsageAdmissionPersistsResolvedEstimateBreakdown(t *testing.T) {
	handler, db, token := workerFixture(t)
	var id int64
	if err := db.QueryRow(`INSERT INTO jobs(repo,issue,title,status,generation,lease_host,lease_epoch,lease_expires_at,view) VALUES('owner/repo',18,'estimate','running',1,'nas',1,now()+interval '1 hour','{}') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	base := map[string]any{"at": "2026-11-01T00:00:00.000Z", "host": "nas", "source": "worker", "repo": "owner/repo", "issue": 18, "provider": "codex", "step": "実装", "inputTokens": 100, "cacheReadTokens": 0, "outputTokens": 0, "elapsedMs": 100}
	missing := map[string]any{"at": "2026-11-01T00:00:01.000Z", "host": "nas", "source": "worker", "repo": "owner/repo", "issue": 18, "provider": "codex", "step": "実装", "usagePartial": true, "elapsedMs": 10}
	fallback := map[string]any{"at": "2026-11-01T00:00:02.000Z", "host": "nas", "source": "worker", "repo": "owner/repo", "issue": 18, "provider": "codex", "step": "実装", "usagePartial": true}
	body, err := json.Marshal(map[string]any{"epoch": 1, "kind": "implementation_usage_check", "data": map[string]any{"rows": []any{base, missing, fallback}, "skipped": 0}})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/jobs/" + fmtInt(id)
	status, result := workerRequest(t, handler, token, "POST", path+"/events", string(body))
	if status != 200 || result["usageLimitReason"] != nil {
		t.Fatalf("check: %d %v", status, result)
	}
	status, detail := workerRequest(t, handler, token, "GET", path, "")
	if status != 200 {
		t.Fatalf("detail: %d %v", status, detail)
	}
	for _, raw := range detail["events"].([]any) {
		event := raw.(map[string]any)
		if event["kind"] != "implementation_usage_estimate" {
			continue
		}
		estimate := event["data"].(map[string]any)["estimate"].(map[string]any)
		steps := estimate["steps"].([]any)
		if len(steps) != 1 {
			t.Fatalf("steps: %v", steps)
		}
		step := steps[0].(map[string]any)
		if step["estimated"] != float64(110) || step["fallbackRows"] != float64(1) {
			t.Fatalf("step: %v", step)
		}
		return
	}
	t.Fatalf("estimate event missing: %v", detail)
}

// 運用資料の集計SQLを、実際の判定イベントと旧形式に対して実行する。
func TestUsageImpactQueryOnPersistedEvents(t *testing.T) {
	handler, db, token := workerFixture(t)
	var id int64
	if err := db.QueryRow(`INSERT INTO jobs(repo,issue,title,status,generation,lease_host,lease_epoch,lease_expires_at,view) VALUES('owner/repo',19,'impact','running',1,'nas',1,now()+interval '1 hour','{}') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	base := map[string]any{"at": "2026-11-01T00:00:00.000Z", "host": "nas", "source": "worker", "repo": "owner/repo", "issue": 19, "provider": "codex", "step": "実装", "inputTokens": 100, "cacheReadTokens": 0, "outputTokens": 0, "elapsedMs": 100}
	untimed := map[string]any{"at": "2026-11-01T00:00:01.000Z", "host": "nas", "source": "worker", "repo": "owner/repo", "issue": 19, "provider": "codex", "step": "実装", "inputTokens": 10, "cacheReadTokens": 0, "outputTokens": 0}
	missing := map[string]any{"at": "2026-11-01T00:00:02.000Z", "host": "nas", "source": "worker", "repo": "owner/repo", "issue": 19, "provider": "codex", "step": "実装", "usagePartial": true, "elapsedMs": 1}
	check := func() map[string]any {
		b, _ := json.Marshal(map[string]any{"epoch": 1, "kind": "implementation_usage_check", "data": map[string]any{"rows": []any{base, untimed, missing}, "skipped": 0}})
		status, result := workerRequest(t, handler, token, "POST", "/api/v1/jobs/"+fmtInt(id)+"/events", string(b))
		if status != 200 {
			t.Fatalf("check: %d %v", status, result)
		}
		return result
	}
	if result := check(); result["usageLimitReason"] != nil {
		t.Fatal(result)
	}
	base["inputTokens"] = 9000000
	if result := check(); result["usageLimitReason"] == nil {
		t.Fatal(result)
	}
	if result := check(); result["usageLimitReason"] == nil {
		t.Fatal(result)
	}
	if _, err := db.Exec(`INSERT INTO job_events(job_id,kind,data) VALUES($1,'implementation_usage_limit','{"estimate":null}')`, id); err != nil {
		t.Fatal(err)
	}
	document, err := os.ReadFile("../../../docs/usage-review-alert-measurement.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(document)
	// 本番では共有DBの public を指定する。fixture は共有DBとアプリDBを別スキーマに隔離する。
	if !strings.Contains(text, "SET search_path TO public;") {
		t.Fatal("shared database search_path missing")
	}
	start := strings.Index(text, "-- usage-impact-query-start")
	end := strings.Index(text, "-- usage-impact-query-end")
	if start < 0 || end <= start {
		t.Fatal("query markers missing")
	}
	query := text[start:end]
	query = strings.ReplaceAll(query, ":'start'", "'2000-01-01T00:00:00Z'")
	query = strings.ReplaceAll(query, ":'end'", "'2100-01-01T00:00:00Z'")
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type counts struct{ events, jobs, fallbackEvents, fallbackJobs, fallbackRows, unknown int }
	got := map[string]counts{}
	for rows.Next() {
		var kind string
		var c counts
		if err := rows.Scan(&kind, &c.events, &c.jobs, &c.fallbackEvents, &c.fallbackJobs, &c.fallbackRows, &c.unknown); err != nil {
			t.Fatal(err)
		}
		got[kind] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got["implementation_usage_estimate"] != (counts{1, 1, 1, 1, 1, 0}) || got["implementation_usage_limit"] != (counts{3, 1, 2, 1, 2, 1}) {
		t.Fatalf("counts: %#v", got)
	}
}
