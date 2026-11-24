package notices

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"regexp"
)

func targets(v any, max int) ([]any, bool) {
	list, ok := v.([]any)
	if !ok || len(list) > max {
		return nil, false
	}
	out := []any{}
	seen := map[string]bool{}
	for _, raw := range list {
		item := fleet.Object(raw)
		if len(item) != 2 || !only(item, "repo", "number") || !fleet.ValidRepo(fleet.Text(item["repo"])) || !integer(item["number"], 1, 9007199254740991) {
			return nil, false
		}
		key := fmt.Sprint(item["repo"], "#", item["number"])
		if !seen[key] {
			out = append(out, item)
			seen[key] = true
		}
	}
	return out, true
}
func (s *Service) ops(st settings, host string, body map[string]any) (any, error) {
	o, err := parseOpsRequest(host, body)
	if err != nil {
		return nil, err
	}
	request := o.request
	receipts, err := st.read("ops-watch-notice-requests")
	if err != nil {
		return nil, err
	}
	if result, found, err := opsReceipt(receipts, body["id"], request); err != nil || found {
		return result, err
	}
	rows, err := st.read("ops-watch-notices")
	if err != nil {
		return nil, err
	}
	rows, result, changed, err := s.upsertOpsNotice(rows, host, o)
	if err != nil {
		return nil, err
	}
	if changed {
		if err = st.save("ops-watch-notices", rows); err != nil {
			return nil, err
		}
	}
	receipts = append(receipts, map[string]any{"id": body["id"], "request": request, "result": result})
	return result, st.save("ops-watch-notice-requests", receipts)
}

// opsRequest は検査した運用の通知の内容。
type opsRequest struct {
	// id と causeValue は検査済みの送信ID・原因（原因が無ければ nil）。
	id, causeValue any
	request        map[string]any
	issues, wanted []any
	cause          string
	hasCause       bool
}

// parseOpsRequest は運用の通知の項目・対象・原因を検査し、受付記録と照合する内容を組み立てる。
func parseOpsRequest(host string, body map[string]any) (opsRequest, error) {
	issues, valid := targets(body["issues"], 1)
	_, hasCause := body["cause"]
	cause := fleet.Text(body["cause"])
	if !only(body, "kind", "id", "text", "reason", "issues", "cause", "targets") || !regexp.MustCompile(`^[a-f0-9-]{36}$`).MatchString(fleet.Text(body["id"])) || !contract.FilledText(body["text"], 1000) || !contract.FilledText(body["reason"], 1000) || !valid || hasCause && !contract.FilledText(cause, 200) {
		return opsRequest{}, problem(400, "運用の通知の形が違います。")
	}
	wanted := []any{}
	if raw, exists := body["targets"]; exists {
		if !hasCause {
			return opsRequest{}, problem(400, "対象の一覧には原因が必要です。")
		}
		var valid bool
		wanted, valid = targets(raw, 50)
		if !valid {
			return opsRequest{}, problem(400, "対象の一覧の形が違います。")
		}
	}
	request := map[string]any{"host": host, "kind": body["kind"], "text": clean(body["text"]), "reason": clean(body["reason"]), "issues": issues}
	if hasCause {
		request["cause"] = cause
		request["targets"] = wanted
	}
	return opsRequest{id: body["id"], causeValue: body["cause"], request: request, issues: issues, wanted: wanted, cause: cause, hasCause: hasCause}, nil
}

// opsReceipt は同じ通知IDの受付記録があれば、同じ内容の送り直しかを照合してその結果を返す。
func opsReceipt(receipts []any, id any, request map[string]any) (any, bool, error) {
	for _, raw := range receipts {
		receipt := fleet.Object(raw)
		if receipt["id"] == id {
			if encode(receipt["request"]) != encode(request) {
				return nil, false, problem(409, "同じ通知IDの内容が違います。")
			}
			return receipt["result"], true, nil
		}
	}
	return nil, false, nil
}

// upsertOpsNotice は通知の行を、同じIDなら照合、同じ原因なら対象を統合、どちらも無ければ新しく足す。行を変えたかを返す。
func (s *Service) upsertOpsNotice(rows []any, host string, o opsRequest) ([]any, any, bool, error) {
	request, issues, wanted, cause, hasCause := o.request, o.issues, o.wanted, o.cause, o.hasCause
	var result any
	changed := false
	for _, raw := range rows {
		row := fleet.Object(raw)
		if row["id"] == o.id {
			if row["host"] != host || row["title"] != request["text"] || row["reason"] != request["reason"] || encode(row["issues"]) != encode(issues) || row["cause"] != o.causeValue {
				return nil, nil, false, problem(409, "同じ通知IDの内容が違います。")
			}
			result = map[string]any{"id": o.id}
			break
		}
	}
	if result == nil && hasCause {
		for _, raw := range rows {
			row := fleet.Object(raw)
			if row["cause"] == cause {
				merged, _ := targets(append(fleet.List(row["targets"]), wanted...), 100)
				if len(merged) > 50 {
					merged = merged[:50]
				}
				row["targets"] = merged
				count := len(merged)
				if count == 0 {
					count = 1
				}
				row["count"] = count
				row["updatedAt"] = contract.ISO(s.now())
				result = map[string]any{"id": row["id"]}
				changed = true
				break
			}
		}
	}
	if result == nil {
		if len(rows) >= 1000 {
			return nil, nil, false, problem(409, "通知が1000件に達しました。")
		}
		row := map[string]any{"id": o.id, "host": host, "title": request["text"], "reason": request["reason"], "issues": issues, "updatedAt": contract.ISO(s.now())}
		if hasCause {
			row["cause"] = cause
			row["targets"] = wanted
			count := len(wanted)
			if count == 0 {
				count = 1
			}
			row["count"] = count
		}
		rows = append(rows, row)
		changed = true
		result = map[string]any{"id": o.id}
	}
	return rows, result, changed, nil
}

func (s *Service) Acknowledge(ctx context.Context, id string) (any, error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err = sqlDialect.BeginLockedWrite(ctx, conn, "ops-watch-notices"); err != nil {
		return nil, err
	}
	defer conn.ExecContext(context.Background(), bindSQL("ROLLBACK"))
	st := settings{conn, ctx}
	rows, err := st.read("ops-watch-notices")
	if err != nil {
		return nil, err
	}
	next := []any{}
	for _, raw := range rows {
		if fleet.Object(raw)["id"] != id {
			next = append(next, raw)
		}
	}
	if err = st.save("ops-watch-notices", next); err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, bindSQL("COMMIT")); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "acknowledged": true}, nil
}
