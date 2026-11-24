package adminapi

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"issue-desk-intake/server/internal/auth"
	"issue-desk-intake/server/internal/contract"
)

func grantStatus(row map[string]any, now time.Time) string {
	if row["revoked_at"] != nil {
		return "revoked"
	}
	if expires, ok := moment(row["expires_at"]); ok && !expires.After(now) {
		return "expired"
	}
	return "active"
}
func grantView(row map[string]any, now time.Time) map[string]any {
	return map[string]any{"id": row["id"], "kind": row["kind"], "target": row["target"], "status": grantStatus(row, now), "permanent": row["expires_at"] == nil, "ttlSeconds": row["ttl_seconds"], "expiresAt": row["expires_at"], "evidence": map[string]any{"quote": row["evidence_quote"], "statedAt": row["evidence_stated_at"]}, "createdBy": row["created_by"], "createdAt": row["created_at"], "revokedAt": row["revoked_at"], "revokedBy": row["revoked_by"], "appliedAt": row["applied_at"]}
}
func listGrantRows(ctx context.Context, tx queryRunner, status, kind string, now time.Time) ([]map[string]any, error) {
	query := "SELECT * FROM permission_grants WHERE 1=1"
	args := []any{}
	if status == "active" {
		// queryMaps formats timestamps to milliseconds before grantStatus compares them.
		query += " AND revoked_at IS NULL AND (expires_at IS NULL OR date_trunc('milliseconds', expires_at) > ?)"
		args = append(args, now)
	}
	if kind != "" {
		query += " AND kind = ?"
		args = append(args, kind)
	}
	return queryMaps(ctx, tx, query+" ORDER BY created_at DESC,id", args...)
}
func (s *service) grants(r *http.Request, p auth.Principal) (any, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := r.Context()
	tx, e := s.beginRepair(ctx)
	if e != nil {
		return nil, 0, e
	}
	defer tx.Rollback()
	if r.Method == http.MethodPost {
		if _, e = tx.ExecContext(ctx, "LOCK TABLE permission_grants IN EXCLUSIVE MODE"); e != nil {
			return nil, 0, e
		}
	}
	now := s.Now()
	id, action, e := idRoute(strings.TrimPrefix(r.URL.Path, "/api/permission-grants"), grantPattern, "経路が見つかりません。")
	if e != nil {
		return nil, 0, e
	}
	if r.Method == http.MethodGet {
		return readGrants(ctx, tx, r, id, action, now)
	}
	if r.Method != http.MethodPost {
		return nil, 0, bad(405, "このメソッドは使えません。")
	}
	changed := false
	status := 200
	if id == "" {
		data, e := body(r)
		if e != nil {
			return nil, 0, e
		}
		request, e := s.grantRequest(data, now)
		if e != nil {
			return nil, 0, e
		}
		same, e := createGrant(ctx, tx, request, p, now)
		if e != nil {
			return nil, 0, e
		}
		if same != nil {
			return grantView(same, now), 200, nil
		}
		id = request.id
		changed = true
		status = 201
		action = "granted"
	} else {
		changed, e = s.changeGrant(ctx, tx, p, id, action, now)
		if e != nil {
			return nil, 0, e
		}
	}
	row, e := queryMap(ctx, tx, "SELECT * FROM permission_grants WHERE id=?", id)
	if e != nil {
		return nil, 0, e
	}
	if e = tx.Commit(); e != nil {
		return nil, 0, e
	}
	if changed {
		s.log("許可の付与の操作を保存しました: " + action + " " + id)
		s.notify("permission-grant", id)
	}
	return grantView(row, now), status, nil
}

// readGrants answers a GET for one grant or for the filtered list.
func readGrants(ctx context.Context, tx queryRunner, r *http.Request, id, action string, now time.Time) (any, int, error) {
	if action != "" {
		return nil, 0, bad(405, "このメソッドは使えません。")
	}
	if id != "" {
		row, e := queryMap(ctx, tx, "SELECT * FROM permission_grants WHERE id=?", id)
		if e != nil {
			return nil, 0, e
		}
		if row == nil {
			return nil, 0, bad(404, "許可の付与が見つかりません。")
		}
		return grantView(row, now), 200, nil
	}
	status, kind := r.URL.Query().Get("status"), r.URL.Query().Get("kind")
	if status == "" {
		status = "active"
	}
	if status != "active" && status != "all" || kind != "" && !grantKind(kind) {
		return nil, 0, bad(400, "一覧の絞り込みを確認してください。")
	}
	rows, e := listGrantRows(ctx, tx, status, kind, now)
	if e != nil {
		return nil, 0, e
	}
	out := []map[string]any{}
	for _, row := range rows {
		out = append(out, grantView(row, now))
	}
	return map[string]any{"grants": out}, 200, nil
}

// grantFields is a validated request to grant a permission.
type grantFields struct {
	id, kind, target string
	ttl, expires     any
	quote            string
	stated           time.Time
}

// grantRequest validates the body of a grant request.
func (s *service) grantRequest(data map[string]any, now time.Time) (grantFields, error) {
	if problem := contract.OnlyKeys(data, "id", "kind", "target", "ttlSeconds", "permanent", "evidence"); problem != nil {
		return grantFields{}, problem
	}
	id := str(data["id"])
	kind, target := str(data["kind"]), str(data["target"])
	if !grantPattern.MatchString(id) || !grantKind(kind) || !s.targetAllowed(kind, target) {
		return grantFields{}, bad(400, "許可の付与の入力を確認してください。")
	}
	if _, exists := data["permanent"]; exists {
		if _, ok := data["permanent"].(bool); !ok {
			return grantFields{}, bad(400, "permanentはtrueかfalseで指定してください。")
		}
	}
	var ttl any = int64(3600)
	var expires any
	permanent := boolValue(data["permanent"])
	if permanent {
		if _, exists := data["ttlSeconds"]; exists || kind != "nas-claude-rule" {
			return grantFields{}, bad(400, "この許可に恒久の設定は使えません。")
		}
		ttl = nil
	} else if v, exists := data["ttlSeconds"]; exists {
		seconds, ok := integer(v)
		if !ok || seconds < 60 || seconds > 86400 {
			return grantFields{}, bad(400, "期限は60〜86400秒で指定してください。")
		}
		ttl = seconds
	}
	if ttl != nil {
		expires = iso(now.Add(time.Duration(ttl.(int64)) * time.Second))
	}
	evidence := obj(data["evidence"])
	if evidence == nil {
		return grantFields{}, bad(400, "許可の根拠を指定してください。")
	}
	if problem := contract.OnlyKeys(evidence, "quote", "statedAt"); problem != nil {
		return grantFields{}, problem
	}
	quote := strings.TrimSpace(str(evidence["quote"]))
	stated, ok := moment(evidence["statedAt"])
	if !ok {
		parsed, parseErr := time.Parse("2006-01-02T15:04Z07:00", str(evidence["statedAt"]))
		stated, ok = parsed, parseErr == nil
	}
	if quote == "" || strings.ContainsRune(quote, 0) || utf8.RuneCountInString(quote) > 2000 || !ok || stated.After(now) || stated.Before(now.Add(-7*24*time.Hour)) {
		return grantFields{}, bad(400, "根拠の引用と7日以内の発言時刻を指定してください。")
	}
	return grantFields{id: id, kind: kind, target: target, ttl: ttl, expires: expires, quote: quote, stated: stated}, nil
}

// createGrant saves a new grant. A repeated request with the same content
// returns the saved grant instead of saving it again.
func createGrant(ctx context.Context, tx *repairTx, g grantFields, p auth.Principal, now time.Time) (map[string]any, error) {
	same, e := queryMap(ctx, tx, "SELECT * FROM permission_grants WHERE id=?", g.id)
	if e != nil {
		return nil, e
	}
	if same != nil {
		sameTime, _ := moment(same["evidence_stated_at"])
		if same["kind"] != g.kind || same["target"] != g.target || !reflect.DeepEqual(same["ttl_seconds"], g.ttl) || same["evidence_quote"] != g.quote || !sameTime.Equal(g.stated) {
			return nil, bad(409, "同じidで内容の違う許可の付与が既にあります。")
		}
		return same, nil
	}
	active, e := queryMaps(ctx, tx, "SELECT * FROM permission_grants WHERE revoked_at IS NULL")
	if e != nil {
		return nil, e
	}
	count := 0
	for _, row := range active {
		if grantStatus(row, now) != "active" {
			continue
		}
		count++
		if row["kind"] == g.kind && row["target"] == g.target {
			return nil, bad(409, "同じ種類と対象の有効な許可の付与が既にあります。")
		}
	}
	if count >= 10 {
		return nil, bad(409, "有効な許可の付与は10件までです。")
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO permission_grants(id,kind,target,ttl_seconds,expires_at,evidence_quote,evidence_stated_at,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?)", g.id, g.kind, g.target, g.ttl, g.expires, g.quote, iso(g.stated), actor(p), iso(now))
	if e != nil {
		return nil, e
	}
	return nil, nil
}

// changeGrant revokes a grant or records that it was applied, and reports whether the row changed.
func (s *service) changeGrant(ctx context.Context, tx *repairTx, p auth.Principal, id, action string, now time.Time) (bool, error) {
	row, e := queryMap(ctx, tx, "SELECT * FROM permission_grants WHERE id=?", id)
	if e != nil {
		return false, e
	}
	if row == nil {
		return false, bad(404, "許可の付与が見つかりません。")
	}
	changed := false
	switch action {
	case "revoke":
		if grantStatus(row, now) == "active" {
			_, e = tx.ExecContext(ctx, "UPDATE permission_grants SET revoked_at=?,revoked_by=? WHERE id=?", iso(now), actor(p), id)
			changed = true
		}
	case "applied":
		if p.Kind == auth.Screen {
			return false, bad(403, "反映済みの印は運用の代行トークンか一時キーだけが記録できます。")
		}
		if grantStatus(row, now) != "active" || !s.targetAllowed(str(row["kind"]), str(row["target"])) {
			return false, bad(409, "現在の方針で有効な許可にだけ反映済みを記録できます。")
		}
		_, e = tx.ExecContext(ctx, "UPDATE permission_grants SET applied_at=COALESCE(applied_at,?) WHERE id=?", iso(now), id)
		changed = row["applied_at"] == nil
	default:
		return false, bad(404, "経路が見つかりません。")
	}
	if e != nil {
		return false, e
	}
	return changed, nil
}
