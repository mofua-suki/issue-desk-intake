package consultationai

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"issue-desk-intake/server/internal/consultations"
)

func (s *Service) DraftAlerts(ctx context.Context) ([]any, error) {
	drafts, err := s.options.Records.ListDraftFields(ctx, consultations.DraftReadAlerts)
	if err != nil {
		return nil, err
	}
	config, err := s.options.Config(ctx)
	if err != nil {
		return nil, err
	}
	result := draftAlertsFromRecords(drafts, config)
	now := time.Now()
	if s.options.Now != nil {
		now = s.options.Now()
	}
	for _, d := range drafts {
		if alert := explicitStopAlert(d, now); alert != nil {
			result = append(result, alert)
		}
	}
	return result, nil
}

func draftAlertsFromRecords(drafts []consultations.Record, config map[string]any) []any {
	result := []any{}
	for _, d := range drafts {
		budget := fleet.Object(d["consultationBudget"])
		if stopped := fleet.Object(budget["stopped"]); stopped != nil {
			limits := fleet.Object(stopped["limits"])
			questions := []string{}
			for _, question := range fleet.Elements(stopped["questions"]) {
				questions = append(questions, fleet.Text(question))
			}
			reason := fmt.Sprintf("相談ID: %s。回数 %s / 上限 %s、量 %s / 上限 %s。最後の未確認事項: %s。%s", fleet.Text(d["id"]), budgetAlertNumber(stopped["calls"]), budgetAlertNumber(limits["calls"]), budgetAlertNumber(stopped["weighted"]), budgetAlertNumber(limits["weighted"]), strings.Join(questions, "、"), fleet.Text(stopped["reply"]))
			alert := Record{"key": "consultation-budget:" + fleet.Text(d["id"]), "kind": "consultation_budget", "project": d["project"], "draftId": d["id"], "consultationId": d["id"], "consultationUrl": nil, "consultationUrlReason": "公開URLを確認できません。", "stopCount": len(fleet.Elements(budget["stops"])), "budgetStop": stopped, "title": d["title"], "state": "waiting", "stateLabel": "相談の呼び出し上限で停止", "startedAt": stopped["at"], "updatedAt": d["updatedAt"], "reason": reason}
			if fleet.Text(alert["title"]) == "" {
				alert["title"] = "相談の呼び出し上限"
			}
			links := consultationBudgetLinks(fleet.Text(d["id"]), fleet.Object(config["intake"])["publicUrl"])
			for key, value := range links {
				alert[key] = value
			}
			notice := "相談URLを生成できません: " + fleet.Text(links["consultationUrlReason"]) + " 受付画面の要対応から対象相談を開き、「上限を足して続行する」を選んでください。"
			if link := fleet.Text(links["consultationUrl"]); link != "" {
				notice = "受付画面でこの相談を開き、「上限を足して続行する」を選んでください: " + link
			}
			alert["reason"] = reason + "\n" + consultationBudgetStopMessage + "\n" + notice
			result = append(result, map[string]any(alert))
		}
		if d["parentSplit"] != nil && d["status"] == "draft" && d["splitScopeHeld"] != nil && d["splitScopeHeld"] != false {
			result = append(result, map[string]any{"key": "split-scope-held:" + fleet.Text(d["id"]), "kind": "split_scope_held", "kindName": "分割子の再分割を停止", "project": d["project"], "draftId": d["id"], "title": d["title"], "state": "waiting", "stateLabel": "担当超過のため起票停止", "startedAt": d["updatedAt"], "updatedAt": d["updatedAt"], "count": 1, "detail": "分割子の再分割は親の兄弟案と重複するため起票できません。担当の依頼をまとめて返信してください。分け直す場合は親の相談を終え、新しい相談で整理してください。"})
		}
	}
	return result
}

const consultationBudgetStopMessage = "相談の呼び出しの上限に達したため停止しました。利用者が受付画面の要対応から続行を選ぶまで受付AIを呼びません。返信・再試行では再開できません。"

func budgetAlertNumber(value any) string {
	switch n := value.(type) {
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return fmt.Sprint(value)
}

// Keep the legacy consultationLinks reasons and URL projection: archived notices bind to this exact meaning.
func consultationBudgetLinks(id string, base any) Record {
	result := Record{"consultationId": id, "consultationUrl": nil, "consultationUrlReason": "受付の公開URLを安全に使用できません。"}
	if !uuid.MatchString(id) {
		result["consultationUrlReason"] = "相談IDが受付画面の形式と一致しません。"
		return result
	}
	if base == nil || base == "" {
		result["consultationUrlReason"] = "受付の公開URLが設定されていません。"
		return result
	}
	value, ok := base.(string)
	if !ok {
		return result
	}
	if strings.ContainsAny(value, "\\?#") || strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || r <= 31 || r == 127 || r == 0xfeff }) {
		return result
	}
	public, err := url.Parse(value)
	if err != nil || !(strings.EqualFold(public.Scheme, "https") || strings.EqualFold(public.Scheme, "http")) || public.Hostname() == "" || public.User != nil {
		return result
	}
	public.Scheme = strings.ToLower(public.Scheme)
	if public.Path == "" {
		public.Path = "/"
	}
	public.Fragment = "/drafts/" + id
	result["consultationUrl"] = public.String()
	result["consultationUrlReason"] = nil
	return result
}

// An explicit stop is never resumed automatically. Escalation only makes the
// existing reason visible so an operator can check its release condition.
func explicitStopAlert(d consultations.Record, now time.Time) map[string]any {
	if d["status"] != "stalled" {
		return nil
	}
	cause, present := d["stallCause"]
	if cause != "explicit" && (present || !strings.HasPrefix(fleet.Text(d["error"]), "相談を停止しました: ")) {
		return nil
	}
	stops := fleet.Elements(d["consultationStops"])
	if len(stops) == 0 {
		return nil
	}
	stop := fleet.Object(stops[len(stops)-1])
	at, err := time.Parse(time.RFC3339Nano, fleet.Text(stop["at"]))
	if err != nil || now.Sub(at) < 24*time.Hour {
		return nil
	}
	elapsed := int64(now.Sub(at) / time.Minute)
	reason := fmt.Sprintf("停止理由: %s。停止した操作者: %s。停止日時: %s。経過: %d時間%d分。停止理由の解除条件を確認し、必要なら相談への返信・再開・終了で対処してください。自動では再開しません。", fleet.Text(stop["reason"]), fleet.Text(stop["by"]), at.In(time.FixedZone("JST", 9*3600)).Format("2006-01-02 15:04:05 MST"), elapsed/60, elapsed%60)
	return map[string]any{"key": "consultation-stop:" + fleet.Text(d["id"]), "kind": "consultation_stop", "kindName": "相談の明示停止が継続", "project": d["project"], "draftId": d["id"], "title": d["title"], "state": "stopped", "stateLabel": "明示停止から24時間以上", "handoff": true, "requiresUser": false, "startedAt": stop["at"], "updatedAt": d["updatedAt"], "reason": reason, "elapsedMinutes": elapsed}
}
