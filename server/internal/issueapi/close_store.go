package issueapi

// クローズ依頼の記録を保存し、一覧として読み出す。

import (
	"context"
	"issue-desk-intake/server/internal/appsql"
	"issue-desk-intake/server/internal/fleet"
)

func (s *Service) saveClose(ctx context.Context, r Object) error {
	r["updatedAt"] = s.now()
	return s.save(ctx, "close_requests", fleet.Text(r["id"]), r)
}

// closeFilter は復号の前にSQLで行を減らす条件。呼び元のGoの判定はそのまま残すので、
// 条件は必ずその判定が通す行の上位集合にする（狭めると依頼を取りこぼす）。
type closeFilter struct {
	where string
	args  []any
}

func closeStatusSQL(statuses string) string {
	return jsonText("json", "$.status") + " IN (" + statuses + ")"
}

// closeOpenFilter は closeRequestOpen が通す、結果の出ていない依頼。
func closeOpenFilter() closeFilter {
	return closeFilter{where: closeStatusSQL("'reviewing','confirm','closing'")}
}

// closeRecoveryFilter は recoverCloseCycle が再開する候補。閉じた依頼は closeRecoveryPending が
// 真になり得るものだけを残す（文字列の "true" も残し、厳密な判定はGoに任せる）。
func closeRecoveryFilter() closeFilter {
	pending := jsonText("json", "$.notePending") + "='true' OR (" + jsonText("json", "$.noteAttempted") + "='true' AND (json::jsonb -> 'notePosted') IS DISTINCT FROM 'true'::jsonb) OR " +
		jsonText("json", "$.prClose.result") + "='failed' OR " + jsonText("json", "$.recoveryPending") + "='true'"
	return closeFilter{where: closeStatusSQL("'reviewing','closing'") + " OR (" + closeStatusSQL("'closed'") + " AND (" + pending + "))"}
}

// closeHistoryFilter は repo の閉じた依頼。番号の照合はGoに残す。
func closeHistoryFilter(repo string) closeFilter {
	return closeFilter{where: closeStatusSQL("'closed'") + " AND " + jsonText("json", "$.project") + "=?", args: []any{repo}}
}

// closeRecords は条件に合うクローズ依頼を新しい順に読む。終わった依頼が大半なので、
// 全件を復号せず要る行だけ読む。json 列は TEXT で、PostgreSQL が jsonb として読めない行
// （\u0000 を含む文字列など）や JSON オブジェクトでない行は条件を掛けずに返し、
// 全件を読んでいたときと同じくGoの復号に成否を決めさせる。
func (s *Service) closeRecords(ctx context.Context, filter closeFilter) ([]Object, error) {
	query := "SELECT json FROM close_requests WHERE CASE WHEN NOT pg_input_is_valid(json, 'jsonb') THEN true WHEN jsonb_typeof(json::jsonb) <> 'object' THEN true ELSE COALESCE(" + filter.where + ", false) END ORDER BY " + rowOrder(true)
	rows, e := s.o.AppDB.QueryContext(ctx, bindSQL(query), filter.args...)
	if e != nil {
		return nil, e
	}
	out, _, e := appsql.JSONObjects[string](rows, appsql.JSONRowsOptions{})
	return out, e
}
