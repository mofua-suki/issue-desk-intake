package filing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"time"
	"unicode/utf16"
)

type BuildConfig struct {
	Config              map[string]any
	Records             *consultations.Records
	Now                 func() time.Time
	Project             func(string) error
	ProviderSettings    func(context.Context, string, Record) (Record, error)
	ProviderLabels      func(context.Context, string, string) ([]int64, error)
	ResolvePreceding    func(context.Context, Record) ([]Record, error)
	CheckCurrent        func(context.Context, Record) error
	DependencyCheckable func(string, string) bool
	ContextEvidence     func(Record) string
	ProxySection        func(context.Context, Record) (string, error)
}

func encoded(v any) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

func utfSize(v string) int { return len(utf16.Encode([]rune(v))) }
func normalizedWork(v any) string {
	switch fleet.Text(v) {
	case "bugfix", "feature", "refactor", "docs", "test", "operations", "investigation":
		return fleet.Text(v)
	}
	return "unknown"
}
func configProvider(config map[string]any, provider string, tier Record) (Record, error) {
	if provider == "" {
		provider = "codex"
	}
	if provider != "codex" && provider != "claude" && provider != "antigravity" {
		return nil, fail(400, "実行先が不正です。")
	}
	model := fleet.Text(config["model"])
	if provider == "claude" {
		model = fleet.Text(config["claudeModel"])
		if model == "" {
			model = "opus"
		}
	}
	if provider == "codex" {
		claude := fleet.Text(config["claudeModel"])
		if claude == "" {
			claude = "opus"
		}
		if mapped := fleet.Text(row(config["codexEquivalents"])[claude]); strings.TrimSpace(mapped) != "" {
			model = mapped
		}
	}
	if provider == "antigravity" {
		model = fleet.Text(row(config["antigravity"])["model"])
	}
	effort := fleet.Text(config["reasoningEffort"])
	if effort == "" {
		effort = "medium"
	}
	if tier != nil && provider != "antigravity" {
		entry := row(row(row(config["implementation"])["tiers"])[fleet.Text(tier["level"])])
		key := "model"
		if provider == "claude" {
			key = "claudeModel"
		}
		if m := fleet.Text(entry[key]); strings.TrimSpace(m) != "" {
			model = m
			if e := fleet.Text(entry["reasoningEffort"]); strings.TrimSpace(e) != "" {
				effort = e
			}
		}
	}
	if model == "" {
		return nil, fail(503, "実行先のモデルが設定されていません。")
	}
	return Record{"provider": provider, "model": model, "effort": effort}, nil
}
func (c BuildConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
func (c BuildConfig) provider(ctx context.Context, provider string, tier Record) (Record, error) {
	if c.ProviderSettings != nil {
		return c.ProviderSettings(ctx, provider, tier)
	}
	return configProvider(c.Config, provider, tier)
}
func imageReviews(d Record) error {
	images := []any{}
	seen := map[string]bool{}
	for _, raw := range fleet.Elements(d["messages"]) {
		for _, im := range fleet.Elements(row(raw)["images"]) {
			id := fleet.Text(row(im)["id"])
			if id != "" && !seen[id] {
				seen[id] = true
				images = append(images, im)
			}
		}
	}
	if len(images) == 0 {
		return nil
	}
	reviews := fleet.Elements(d["imageReviews"])
	if len(reviews) != len(images) {
		return fail(409, "受付AIの画像確認が不足しています。画像を保持しているので再試行してください。")
	}
	matched := map[string]bool{}
	for _, raw := range reviews {
		r := row(raw)
		id := fleet.Text(r["id"])
		if !seen[id] || matched[id] || r["readable"] != true {
			return fail(409, "添付画像の確認結果が一致しません。")
		}
		matched[id] = true
		for _, key := range []string{"screen", "location", "change", "uncertainty"} {
			value, ok := r[key].(string)
			if !ok || utfSize(value) > 2000 || key != "uncertainty" && strings.TrimSpace(value) == "" {
				return fail(409, "受付AIが添付画像を読み取れませんでした。画像または依頼文を確認してください。")
			}
		}
	}
	return nil
}
func estimate(v any) (int64, int64, bool) {
	r := row(v)
	lines, a := integer(r["lines"])
	files, b := integer(r["files"])
	return lines, files, a && b && lines >= 0 && files >= 0
}
func bodyLimit(config map[string]any) int {
	n, ok := integer(row(config["intake"])["bodyLimit"])
	if ok && n > 0 {
		return int(n)
	}
	return 60000
}
func issueContext(context Record) string {
	if context == nil {
		return ""
	}
	lines := []string{fmt.Sprintf("受付で確認したGitea資料: %s / commit %s", fleet.Text(context["branch"]), fleet.Text(context["revision"]))}
	for _, v := range fleet.Elements(context["files"]) {
		f := row(v)
		lines = append(lines, fmt.Sprintf("- %s (SHA-256: %s)", fleet.Text(f["path"]), fleet.Text(f["sha256"])))
	}
	if parts := fleet.Elements(context["parts"]); len(parts) > 0 {
		lines = append(lines, "部分的に読んだ資料（全文は未確認。行番号は1始まりで両端を含む）:")
		for _, v := range parts {
			p := row(v)
			lines = append(lines, fmt.Sprintf("- %s (SHA-256: %s、要求条件と返却範囲: %s)", fleet.Text(p["path"]), fleet.Text(p["sha256"]), encoded(p)))
		}
	}
	for _, v := range fleet.Elements(context["refused"]) {
		p := row(v)
		lines = append(lines, fmt.Sprintf("未確認の要求: %s / %s", fleet.Text(p["path"]), encoded(p)))
	}
	for _, key := range []string{"files", "parts"} {
		for _, v := range fleet.Elements(row(context["references"])[key]) {
			p := row(v)
			lines = append(lines, fmt.Sprintf("受付で参照したコントローラー資料: %s（版 %s）", fleet.Text(p["path"]), fleet.Text(p["version"])))
		}
	}
	return strings.Join(lines, "\n")
}
