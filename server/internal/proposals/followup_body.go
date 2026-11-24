package proposals

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/jobs"
)

func inert(value string) string { return strings.ReplaceAll(value, "<!--", "&lt;!--") }

func setFollowupPriority(d consultations.Record, p Object, at string) {
	if priority := obj(p["priority"]); priority != nil {
		d["priority"] = "high"
		d["prioritySource"] = Object{"via": "proposal", "reason": priority["reason"], "at": at}
	}
}

func followupSettings(p Object, provider any, tier filing.Record) Object {
	marker := Object{"provider": provider, "workType": "bugfix"}
	if obj(p["priority"]) != nil {
		marker["priority"] = "high"
	}
	if tier != nil {
		marker["tier"], marker["tierSource"] = tier["level"], tier["source"]
	}
	return marker
}

// The parser retains items as ordered structs for its public fingerprint; JSON
// round-tripping also supports the actual map representation reloaded from disk.
func followupItems(p Object) []Object {
	raw, _ := json.Marshal(p["items"])
	items := []Object{}
	_ = json.Unmarshal(raw, &items)
	return items
}

func followupBody(p Object, desk any, sourceURL string) string {
	sections := []string{fmt.Sprintf("## マージ済みPR\n%s\n\n元Issue: 共通番号%d\n%s", str(p["pr"]), num(desk), sourceURL)}
	items := followupItems(p)
	for _, section := range []struct{ heading, key string }{{"指摘", "problem"}, {"期待する動作と直し方", "expected"}, {"受入条件（テスト方法）", "test"}} {
		lines := []string{"## " + section.heading}
		for i, item := range items {
			// Seven spaces = the ordered list's three plus four for a literal
			// code block. Headings/fences inside review text remain plain text.
			value := strings.ReplaceAll(inert(str(item[section.key])), "\r\n", "\n")
			value = strings.ReplaceAll(value, "\r", "\n")
			lines = append(lines, fmt.Sprintf("%d.\n\n       %s", i+1, strings.ReplaceAll(value, "\n", "\n       ")))
		}
		sections = append(sections, strings.Join(lines, "\n\n"))
	}
	return strings.Join(sections, "\n\n")
}

// followupTier は後続案の実装の難易度を起票と同じ規則（filing.DecideFollowupTier）で決める。
// 受入条件は指摘ごとのテスト方法、変更範囲は案の changeScope.paths。
func followupTier(config Object, p Object) filing.Record {
	paths := []string{}
	for _, v := range arr(obj(p["changeScope"])["paths"]) {
		paths = append(paths, str(v))
	}
	acceptance := 0
	for _, item := range followupItems(p) {
		if strings.TrimSpace(str(item["test"])) != "" {
			acceptance++
		}
	}
	return filing.DecideFollowupTier(filing.BuildConfig{Config: config}, paths, acceptance)
}

// FollowupTier は起票前に実装モデルを選ぶための難易度（難易度の設定が無ければ空文字）。
func (s *Service) FollowupTier(ctx context.Context, d consultations.Record) (string, error) {
	mark, err := s.read(ctx, str(obj(d["proposal"])["key"]))
	if err != nil {
		return "", err
	}
	config, err := s.o.Config()
	if err != nil {
		return "", err
	}
	return str(followupTier(config, obj(mark["proposal"]))["level"]), nil
}

// PrepareFollowup supplies the existing filing outbox with a deterministic body
// and job, without a consultation rewriting or omitting the review findings.
// settings の tier は FollowupTier で選んだ難易度で、受付が決め直した難易度と一致しなければ起票しない。
func (s *Service) PrepareFollowup(ctx context.Context, d consultations.Record, settings Object, labels []int64) (filing.Prepared, error) {
	if err := s.ValidateDraft(ctx, d); err != nil {
		return filing.Prepared{}, err
	}
	mark, err := s.read(ctx, str(obj(d["proposal"])["key"]))
	if err != nil {
		return filing.Prepared{}, err
	}
	p := obj(mark["proposal"])
	if settings["provider"] != d["provider"] || str(settings["model"]) == "" || str(settings["effort"]) == "" {
		return filing.Prepared{}, fail(503, "後続案の実行モデルを確認できません。")
	}
	config, err := s.o.Config()
	if err != nil {
		return filing.Prepared{}, err
	}
	tier := followupTier(config, p)
	if str(settings["tier"]) != str(tier["level"]) {
		return filing.Prepared{}, fail(503, "後続案の実装の難易度が実行モデルの選択時と一致しません。")
	}
	body := str(d["body"])
	deps := arr(p["dependsOn"])
	if len(deps) > 0 {
		raw, _ := json.Marshal(deps)
		body += "\n\n先行IssueのPRマージを待って開始します。\n<!-- issue-desk-dependencies:" + string(raw) + " -->"
	}
	scope := obj(p["changeScope"])
	var paths []string
	if scope != nil {
		paths = []string{}
		for _, v := range arr(scope["paths"]) {
			paths = append(paths, str(v))
		}
	}
	if len(paths) > 0 {
		normalized, err := jobs.NormalizeScope(paths)
		if err != nil {
			return filing.Prepared{}, err
		}
		raw, _ := json.Marshal(Object{"v": 1, "paths": normalized})
		body += "\n\n## 変更範囲（受付の宣言）\n"
		if len(normalized) <= 40 {
			for _, path := range normalized {
				body += fmt.Sprintf("- `%s`\n", path)
			}
		} else {
			body += "- 変更対象は直後のJSONマーカーを参照してください。\n"
		}
		body += "<!-- issue-desk-scope:" + string(raw) + " -->"
	}
	marker := followupSettings(p, d["provider"], tier)
	metadata, _ := json.Marshal(marker)
	body += fmt.Sprintf("\n\n---\n受付: 開発受付 / %s\n実行先: %s / 実装モデル: %s / 推論: %s%s\n<!-- issue-desk:followup -->\n<!-- issue-desk:%s -->\n<!-- issue-desk:settings %s -->", str(d["project"]), str(d["provider"]), str(settings["model"]), str(settings["effort"]), filing.TierDescription(tier), str(d["id"]), metadata)
	job, err := filing.JobFromIssue(str(d["project"]), Object{"number": 1, "title": d["title"], "body": body}, "followup")
	if err != nil {
		return filing.Prepared{}, err
	}
	// The real issue number is attached atomically by filing.complete.
	job.Issue = 0
	prepared := filing.Prepared{Title: str(d["title"]), Body: body, Labels: labels, Job: job}
	if tier != nil {
		prepared.Fields = filing.Record{"implementationTier": tier}
	}
	return prepared, nil
}
