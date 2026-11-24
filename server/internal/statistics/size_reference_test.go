package statistics

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSizeReferenceCalendarBoundedAndMissing(t *testing.T) {
	p := ReferencePeriod(time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC), 30)
	if p.From.In(JST).Format("2006-01-02") != "2026-10-04" || p.To.In(JST).Format("2006-01-02") != "2026-11-03" {
		t.Fatal(p)
	}
	issues := []AccountRecord{}
	for n := 0; n < 100; n++ {
		issues = append(issues, AccountRecord{"repo": "o/r", "size": AccountRecord{"lineBand": strings.Repeat("x", n+1), "fileBand": "不明"}, "unknownGenerations": 1, "total": AccountRecord{"weighted": float64(100), "unknown": AccountRecord{"weighted": 1}, "partialRows": 0}})
	}
	text := SizeReferenceText("o/r", p, issues, nil)
	if len(text) > 6000 || !strings.Contains(text, "省略:") || !strings.Contains(text, "選択済みプロジェクト") || !strings.Contains(text, "平均 不明") {
		t.Fatal(len(text), text)
	}
	missing, e := (&Service{}).SizeReference(context.Background(), "o/r")
	if e != nil || !strings.Contains(missing, "共有DBに接続していません") {
		t.Fatal(missing, e)
	}
}
func TestPostgresSizeAndMeasurementReferences(t *testing.T) {
	db, ctx := testPostgres(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var id int64
	e := db.QueryRowContext(ctx, `INSERT INTO jobs(repo,issue,status,pr,filed_at,result,view)VALUES('o/r',1,'merged',2,$1,'{"mergeCommit":"abc"}','{"estimate":{"lines":20,"files":2}}')RETURNING id`, now.Add(-time.Hour)).Scan(&id)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.ExecContext(ctx, `INSERT INTO job_events(job_id,kind,at,data)VALUES($1,'state',$2,'{"to":"merged"}')`, id, now); e != nil {
		t.Fatal(e)
	}
	row := map[string]any{"source": "worker", "host": "nas", "repo": "o/r", "issue": float64(1), "step": "独立レビュー", "provider": "codex", "model": "m", "generation": float64(1), "at": ISO(now.Add(-time.Minute)), "inputTokens": float64(100), "cacheReadTokens": float64(0), "outputTokens": float64(10), "reviewDiffLines": float64(40), "reviewDiffFiles": float64(2)}
	raw, _ := json.Marshal(row)
	if _, e = db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,source,repo,issue,step,provider,model,json,sender)VALUES('size-ref',$1,'nas','worker','o/r',1,'独立レビュー','codex','m',$2,'test')`, now.Add(-time.Minute), string(raw)); e != nil {
		t.Fatal(e)
	}

	draft := AccountRecord{"source": "intake", "host": "nas", "repo": "o/r", "draft": "d1", "step": "受付の相談", "provider": "codex", "model": "m", "at": ISO(now.Add(-time.Hour)), "inputTokens": float64(50), "cacheReadTokens": float64(0), "outputTokens": float64(0)}
	draftRaw, _ := json.Marshal(draft)
	if _, e = db.ExecContext(ctx, `INSERT INTO usage_rows(key,at,host,source,repo,issue,step,provider,model,json,sender)VALUES('size-draft',$1,'nas','intake','o/r',NULL,'受付の相談','codex','m',$2,'test')`, now.Add(-time.Hour), string(draftRaw)); e != nil {
		t.Fatal(e)
	}
	s := Service{DB: db, Now: func() time.Time { return now }, DraftIssue: func(id string) (string, int64) {
		if id == "d1" {
			return "o/r", 1
		}
		return "", 0
	}}

	text, e := s.SizeReference(ctx, "o/r")
	if e != nil || !strings.Contains(text, "依頼1件") || !strings.Contains(text, "2倍以上1件") || !strings.Contains(text, "200（1件）") {
		t.Fatal(text, e)
	}
	text, e = s.MeasurementReference(ctx, "o/r")
	if e != nil || !strings.Contains(text, "DB使用量: 取得成功") || !strings.Contains(text, "weighted=200") {
		t.Fatal(text, e)
	}
	stop, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = s.MeasurementReference(stop, "o/r"); e == nil {
		t.Fatal("停止が無視されました")
	}
}
