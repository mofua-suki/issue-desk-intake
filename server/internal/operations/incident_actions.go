package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type IncidentActions struct {
	Service            *Service
	Judge              func(context.Context, []any) (map[string]any, error)
	ReadTarget         func(context.Context, string, int64) (map[string]any, error)
	Send               func(context.Context, string, int64, string, string, string) error
	ValidateSlowSource func(context.Context, *SlowIncidentFiling) error
}

func incidentText(v any, limit int) bool {
	t, ok := v.(string)
	return ok && strings.TrimSpace(t) != "" && contract.UTF16Length(t) <= limit && !regexp.MustCompile(`(?i)^\s*/codex\b`).MatchString(t)
}

var causeDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[0-9:.+-]+Z?`)
var causeSHA = regexp.MustCompile(`(?i)\b[0-9a-f]{40}\b`)
var causeUUID = regexp.MustCompile(`(?i)\b[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\b`)
var incidentRefs = regexp.MustCompile(`(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#([0-9]+)`)

func incidentCause(detail map[string]any) string {
	cause := fleet.Text(detail["error"])
	if cause == "" {
		for _, raw := range fleet.List(detail["comments"]) {
			c := fleet.Object(raw)
			if c["bot"] == true && c["kind"] == "ask" {
				cause = fleet.Text(c["body"])
			}
		}
	}
	cause = causeDate.ReplaceAllString(cause, "<日時>")
	cause = causeSHA.ReplaceAllString(cause, "<SHA>")
	cause = causeUUID.ReplaceAllString(cause, "<ID>")
	return strings.Join(strings.Fields(cause), " ")
}
func (a *IncidentActions) read(ctx context.Context, target map[string]any) (map[string]any, error) {
	if target == nil || fleet.Text(target["project"]) == "" || fleet.Number(target["number"]) < 1 {
		return nil, nil
	}
	if a.ReadTarget == nil {
		return nil, fmt.Errorf("対象の記録を読み取れません")
	}
	r, e := a.ReadTarget(ctx, fleet.Text(target["project"]), int64(fleet.Number(target["number"])))
	if e != nil {
		return nil, e
	}
	if r["project"] != target["project"] || fleet.Number(r["number"]) != fleet.Number(target["number"]) {
		return nil, fmt.Errorf("対象の依頼を照合できません")
	}
	return r, nil
}
func (a *IncidentActions) Execute(ctx context.Context, id string) error {
	s := a.Service
	if a.Judge == nil {
		return contract.NewProblem(503, "有事の判断を開始できません。", "")
	}
	var row map[string]any
	claimed := false
	e := s.changeIncident(ctx, id, func(r map[string]any) error {
		if r["state"] != "expired" || r["actionStartedAt"] != nil || r["actionInvokedAt"] != nil {
			return nil
		}
		r["actionInvokedAt"] = iso(s.now())
		r["state"] = "acting"
		r["actionStartedAt"] = iso(s.now())
		r["actions"] = []any{}
		s.incidentNote(r, "期限切れの入口", "期限切れを記録しました。保存後に許可一覧の対処を1回だけ判断します。")
		s.incidentNote(r, "対処開始", "判断開始を保存します。再起動後は再判断・再送しません。")
		encoded, e := json.Marshal(r)
		if e != nil {
			return e
		}
		e = json.Unmarshal(encoded, &row)
		claimed = e == nil
		return e
	})
	if e != nil || !claimed {
		return e
	}
	runErr := a.run(ctx, row)
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	e = s.changeIncident(saveCtx, id, func(r map[string]any) error {
		if runErr != nil {
			s.incidentNote(r, "対処終了理由", runErr.Error())
		}
		r["state"] = "acted"
		s.incidentNote(r, "対処終了", "判断・送信の再試行は行いません。")
		return nil
	})
	return e
}
func (a *IncidentActions) run(ctx context.Context, row map[string]any) error {
	s := a.Service
	id := fleet.Text(row["id"])
	target := fleet.Object(row["target"])
	detail, e := a.read(ctx, target)
	if e != nil {
		return e
	}
	if detail["stopped"] == true {
		return fmt.Errorf("対象の依頼に明示された停止があります")
	}
	item := map[string]any{"key": row["key"], "kind": row["kind"], "project": target["project"], "number": target["number"], "summary": map[string]any{"title": row["title"]}, "detail": detail}
	result, e := a.Judge(ctx, []any{redactIncidentData(item)})
	if e != nil {
		return e
	}
	actions, ok := result["actions"].([]any)
	if len(result) != 1 || !ok || len(actions) > 5 {
		return fmt.Errorf("AIの行動一覧の形が不正です")
	}
	note := func(name, reason string) error {
		return s.changeIncident(ctx, id, func(r map[string]any) error { s.incidentNote(r, name, reason); return nil })
	}
	for _, raw := range actions {
		action := fleet.Object(raw)
		name := fleet.Text(action["action"])
		if problem := incidentActionProblem(action, item, target); problem != "" {
			if e = note("行動拒否", problem); e != nil {
				return e
			}
			continue
		}
		if name == "none" {
			if e = note("見送り", fmt.Sprintf("%v 根拠: %v", action["reason"], action["evidence"])); e != nil {
				return e
			}
			continue
		}
		fresh, e := a.read(ctx, target)
		if e != nil {
			return e
		}
		rejection := incidentSendRejection(name, action, detail, fresh)
		if rejection != "" {
			if e = note("行動拒否", rejection); e != nil {
				return e
			}
			continue
		}
		reservationID, e := requestID()
		if e != nil {
			return e
		}
		reserved := false
		e = s.changeIncident(ctx, id, func(r map[string]any) error {
			for _, raw := range fleet.List(r["actions"]) {
				if fleet.Object(raw)["action"] == name {
					s.incidentNote(r, "行動拒否", "同じ有事の同じ行動は1回までです。")
					return nil
				}
			}
			r["actions"] = append(fleet.List(r["actions"]), map[string]any{"id": reservationID, "action": name, "day": s.now().In(time.FixedZone("JST", 9*3600)).Format("2006-01-02"), "reason": usage.Redact(fleet.Text(action["reason"])), "evidence": usage.Redact(fleet.Text(action["evidence"])), "result": "送信予約（応答不明でも再送しない）"})
			s.incidentNote(r, "行動予約", fmt.Sprintf("%s 送信ID %s: %v 根拠: %v", name, reservationID, action["reason"], action["evidence"]))
			reserved = true
			return nil
		})
		if e != nil {
			return e
		}
		if !reserved {
			continue
		}
		var sendErr error
		if name == "notify" {
			sendErr = note("お知らせ", fleet.Text(action["text"]))
		} else if a.Send == nil {
			sendErr = fmt.Errorf("送信先がありません")
		} else {
			sendErr = a.Send(ctx, fleet.Text(target["project"]), int64(fleet.Number(target["number"])), reservationID, name, fleet.Text(action["text"]))
		}
		outcome := "成功"
		if sendErr != nil {
			outcome = "失敗（再送しない）: " + usage.Redact(sendErr.Error())
		}
		e = s.changeIncident(ctx, id, func(r map[string]any) error {
			for _, raw := range fleet.List(r["actions"]) {
				reservation := fleet.Object(raw)
				if reservation["id"] == reservationID {
					reservation["result"] = outcome
				}
			}
			s.incidentNote(r, "行動結果", fmt.Sprintf("%s 送信ID %s: %s", name, reservationID, outcome))
			return nil
		})
		if e != nil {
			return e
		}
	}
	return nil
}

// incidentActionProblem は判断AIの行動1件が、決まった形・対象の依頼だけを指す本文かを検査し、断る理由を返す。問題が無ければ空を返す。
func incidentActionProblem(action, item, target map[string]any) string {
	name := fleet.Text(action["action"])
	validName := name == "retry" || name == "reply" || name == "notify" || name == "none"
	_, textOK := action["text"].(string)
	_, evidenceOK := action["evidence"].(string)
	requiresUser, uok := action["requiresUser"].(bool)
	_, rok := action["resolved"].(bool)
	if len(action) != 7 || contract.OnlyKeys(action, "action", "item", "reason", "text", "evidence", "resolved", "requiresUser") != nil || !validName || action["item"] != item["key"] || !incidentText(action["reason"], 1000) || !textOK || !evidenceOK || !uok || !rok {
		return "許可されない行動・余分な項目・対象・形式"
	}
	if requiresUser && name != "notify" {
		return "利用者の判断は notify だけです。"
	}
	if name != "none" && !incidentText(action["text"], 20000) {
		return "本文を確認できません。"
	}
	for _, match := range incidentRefs.FindAllStringSubmatch(fleet.Text(action["text"]), -1) {
		repo := match[1]
		if repo == "" {
			repo = fleet.Text(target["project"])
		}
		number, err := strconv.ParseInt(match[2], 10, 64)
		if target == nil || repo != target["project"] || err != nil || float64(number) != fleet.Number(target["number"]) {
			return "本文の未確認の #番号"
		}
	}
	return ""
}

// incidentSendRejection は送る直前の依頼の状態が、判断した行動をまだ受け付けるかを判定し、断る理由を返す。
func incidentSendRejection(name string, action, detail, fresh map[string]any) string {
	resolved, _ := action["resolved"].(bool)
	rejection := ""
	if fresh["stopped"] == true {
		rejection = "対象の依頼に明示された停止があります。"
	}
	if name == "retry" && (fresh == nil || (fresh["status"] != "failed" && fresh["status"] != "review_held") || fresh["state"] == "closed" || !resolved || !incidentText(action["evidence"], 4000) || incidentCause(fresh) == "" || incidentCause(fresh) != incidentCause(detail)) {
		rejection = "再試行の状態・原因の解消根拠を確認できません。"
	}
	if name == "reply" {
		before, _ := json.Marshal(detail["comments"])
		after, _ := json.Marshal(fresh["comments"])
		if fresh == nil || fresh["status"] != "waiting" || fresh["state"] == "closed" || !incidentText(action["evidence"], 4000) || string(before) != string(after) {
			rejection = "実装の回答待ち・回答の根拠を確認できません。"
		}
	}
	return rejection
}

func redactIncidentData(value any) any {
	switch v := value.(type) {
	case string:
		return usage.Redact(v)
	case map[string]any:
		out := map[string]any{}
		for k, child := range v {
			out[k] = redactIncidentData(child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = redactIncidentData(child)
		}
		return out
	default:
		return value
	}
}
