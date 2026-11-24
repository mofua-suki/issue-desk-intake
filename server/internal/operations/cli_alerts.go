package operations

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/hosts"
	"issue-desk-intake/server/internal/usage"
	"strings"
	"time"
)

func (s *Service) cliAndChecks(ctx context.Context) ([]any, error) {
	rows, e := s.hostRows(ctx)
	if e != nil {
		return nil, e
	}
	out := []any{}
	assigned := map[string]bool{}
	if s.Assignments != nil {
		for _, list := range s.Assignments.Implementers {
			for _, h := range list {
				assigned[h] = true
			}
		}
		for _, v := range fleet.Object(s.Assignments.Output["repositories"]) {
			assigned[fleet.Text(v)] = true
		}
	}
	for _, r := range rows {
		host := fleet.Text(r["host"])
		if assigned[host] && fleet.Object(r["capabilities"])["workerTokenKind"] == "legacy" {
			out = append(out, map[string]any{"id": "worker-token-" + host, "kind": "monitor", "status": "worker_token_legacy", "project": fleet.SystemProject, "host": host, "title": "従来トークンで監視しています（" + host + "）", "detail": "PC: " + host + "\n最新の生存報告では従来のGiteaトークンを使用しています。監視用アカウント以外の案コメントは拒否されます。\n管理者が監視用トークンを安全に配置し、監視を起動し直してください（skills/add-implementer-pc/SKILL.md の手順5）。監視用トークンで次の生存報告が届くと自動で消えます。\nmofua-suki を intake.proposalWriters に追加しないでください。"})
		}
		out = append(out, cliVersionAlerts(host, r)...)
		if alert := s.hostCheckAlert(host, r); alert != nil {
			out = append(out, alert)
		}
	}
	return out, nil
}

// cliVersionAlerts はPCのCLIの版がイメージの定義と違うものを要対応にする。
func cliVersionAlerts(host string, r map[string]any) []any {
	out := []any{}
	if hosts.ValidCLIVersions(r["cli_versions"]) {
		for _, raw := range fleet.List(r["cli_versions"]) {
			v := fleet.Object(raw)
			reason := fleet.Text(v["reason"])
			if reason == "" && v["current"] == v["expected"] {
				continue
			}
			current := fleet.Text(v["current"])
			if current == "" {
				current = "不明"
			}
			line := fmt.Sprintf("PC %s: %v 今 %s / イメージの定義 %v", host, v["cli"], current, v["expected"])
			if reason == "" {
				reason = "ほかのPCとCLIの版が違います"
			}
			out = append(out, map[string]any{"id": "cli-version-" + host + "-" + fleet.Text(v["cli"]), "kind": "cli_version", "status": "stall_alert", "project": fleet.SystemProject, "host": host, "detectedAt": iso(time.UnixMilli(int64(fleet.Number(v["at"])))), "title": "CLIの版がイメージと違います（" + usage.Redact(line) + "）", "detail": usage.Redact(line + "\nイメージを入れ替えてください。" + reason)})
		}
	}
	return out
}

// hostCheckAlert は直近15分に生存報告があるPCの起動時の自己検査で、通っていない項目を要対応にする。該当しなければ nil を返す。
func (s *Service) hostCheckAlert(host string, r map[string]any) map[string]any {
	at, e := time.Parse(time.RFC3339Nano, fleet.Text(r["heartbeat_at"]))
	if e != nil || at.After(s.now()) || s.now().Sub(at) > 15*time.Minute {
		return nil
	}
	checks := fleet.Object(r["checks"])
	items, ok := checks["items"].([]any)
	if !ok {
		return nil
	}
	detail := []string{"PC: " + host}
	for _, raw := range items {
		item := fleet.Object(raw)
		if item["ok"] == false {
			label := fleet.Text(item["label"])
			if label == "" {
				label = fleet.Text(item["name"])
			}
			detail = append(detail, "- "+label+": "+fleet.Text(item["detail"]))
		}
	}
	if len(detail) == 1 {
		return nil
	}
	detected := checks["at"]
	if _, e := time.Parse(time.RFC3339Nano, fleet.Text(detected)); e != nil {
		detected = iso(at)
	}
	if r["ready"] == false {
		detail = append(detail, "受付はこのPCに仕事を配りません（ready=false）。")
	}
	detail = append(detail, fmt.Sprintf("%s の監視の起動ログと自己検査結果を確認し、原因を直して監視を起動し直すと消えます。", host))
	return map[string]any{"id": "host-check-" + host, "kind": "host_drift", "status": "stall_alert", "project": fleet.SystemProject, "host": host, "detectedAt": detected, "title": "起動時の自己検査が通っていません（" + host + "）", "detail": usage.Redact(strings.Join(detail, "\n"))}
}
