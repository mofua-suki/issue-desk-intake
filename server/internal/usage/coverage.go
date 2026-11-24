package usage

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"time"
)

const coverageScope = "受付DBで取得した全PC分の記録。全PCの全呼び出しの完全性は不明。"
const coverageCaution = "24時間以上は送信停止の疑い。AI未実行・PC停止・配送遅延と区別不能。削減効果とは判定しない。"

func date(v time.Time) string { return contract.ISO(v) }
func state(latest string, now time.Time) string {
	if latest == "" {
		return "記録未到着"
	}
	at, e := time.Parse(time.RFC3339Nano, latest)
	if e != nil {
		return "取得失敗で不明"
	}
	if now.Sub(at) >= 24*time.Hour {
		return "送信停止の疑い"
	}
	return "24時間未満"
}
func contains(values []string, v string) bool {
	for _, value := range values {
		if value == v {
			return true
		}
	}
	return false
}
func (s *Store) Coverage(ctx context.Context, from, to, now time.Time, hosts, senders []string) (map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT host,sender,source,count(*),max(at)FROM usage_rows WHERE at>=$1 AND at<$2 GROUP BY host,sender,source ORDER BY host,sender,source", from, to)
	if err != nil {
		return nil, err
	}
	groups := []any{}
	type group struct {
		host, sender, latest string
		count                int
	}
	raw := []group{}
	total := 0
	for rows.Next() {
		var g group
		var source *string
		var latest time.Time
		if err = rows.Scan(&g.host, &g.sender, &source, &g.count, &latest); err != nil {
			rows.Close()
			return nil, err
		}
		g.latest = date(latest)
		raw = append(raw, g)
		total += g.count
		var registeredSender any
		if senders != nil {
			registeredSender = contains(senders, g.sender)
		}
		groups = append(groups, map[string]any{"host": SafeText(g.host), "sender": SafeText(g.sender), "source": source, "count": g.count, "latest": g.latest, "status": state(g.latest, now), "registeredHost": contains(hosts, g.host), "registeredSender": registeredSender, "mapping": "不明", "sendingPosition": "不明"})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	registrations := []any{}
	for _, kind := range []string{"host", "sender"} {
		names := hosts
		if kind == "sender" {
			names = senders
		}
		for _, name := range names {
			latest := ""
			count := 0
			for _, g := range raw {
				match := g.host
				if kind == "sender" {
					match = g.sender
				}
				if match == name {
					count += g.count
					if g.latest > latest {
						latest = g.latest
					}
				}
			}
			var at any
			if latest != "" {
				at = latest
			}
			registrations = append(registrations, map[string]any{"kind": kind, "name": SafeText(name), "latest": at, "count": count, "status": state(latest, now)})
		}
	}
	positions := []any{}
	rows, err = s.DB.QueryContext(ctx, "SELECT position,updated_at FROM usage_import_positions ORDER BY file")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var position int64
		var at time.Time
		if err = rows.Scan(&position, &at); err != nil {
			rows.Close()
			return nil, err
		}
		positions = append(positions, map[string]any{"file": fmt.Sprintf("受付ファイル%d", len(positions)+1), "position": position, "updatedAt": date(at), "mapping": "不明"})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	failures := []any{}
	if senders == nil {
		failures = append(failures, "登録記録元: 取得できませんでした")
	}
	return map[string]any{"from": date(from), "to": date(to), "generatedAt": date(now), "scope": coverageScope, "caution": coverageCaution, "ok": len(failures) == 0, "failures": failures, "rows": groups, "registrations": registrations, "imports": positions, "reconciliation": map[string]any{"calls": nil, "recorded": total, "missing": nil, "unmatchable": total, "reason": "母数不明（全経路の呼び出しを一意に照合する実行台帳がない）。記録なし件数は不明。トークン項目の不明は行の欠落と別。"}}, nil
}
func CoverageText(report map[string]any) string {
	lines := []string{fmt.Sprint("到着棚卸し ", report["from"], "〜", report["to"]), coverageScope, coverageCaution, "受付の取り込み位置（送信位置ではない）。各PCの送信位置・hostとsenderとファイルの対応は不明。"}
	r := fleet.Object(report["reconciliation"])
	lines = append(lines, fmt.Sprintf("呼び出し件数=母数不明 記録あり=%v行 記録なし=不明 照合不能=%v行。トークン項目の不明とは別。", r["recorded"], r["unmatchable"]))
	rows := fleet.List(report["rows"])
	selected := 0
	for _, raw := range rows {
		g := fleet.Object(raw)
		line := fmt.Sprintf("host=%v sender=%v source=%v %v行 最新at=%v %v", g["host"], g["sender"], g["source"], g["count"], g["latest"], g["status"])
		if len(strings.Join(lines, "\n"))+len(line) > 1400 {
			break
		}
		lines = append(lines, line)
		selected++
	}
	lines = append(lines, fmt.Sprintf("省略%d件", len(rows)-selected))
	return SafeText(strings.Join(lines, "\n"))
}
