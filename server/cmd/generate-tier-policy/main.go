// generate-tier-policy updates policy projections without reading runtime Skill files.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"issue-desk-intake/server/internal/tierpolicy"
)

const begin = "<!-- issue-desk:tier-policy:begin -->"
const end = "<!-- issue-desk:tier-policy:end -->"

func section(data []byte, content string) ([]byte, error) {
	text := string(data)
	if strings.Count(text, begin) != 1 || strings.Count(text, end) != 1 {
		return nil, fmt.Errorf("生成境界は各1件必要です")
	}
	a, b := strings.Index(text, begin), strings.Index(text, end)
	if a > b {
		return nil, fmt.Errorf("生成境界の順序が逆です")
	}
	return []byte(text[:a+len(begin)] + "\n正本: `server/internal/tierpolicy/policy.go`。`cd server && go run ./cmd/generate-tier-policy --root ..` で生成する。\n\n" + content + "\n" + text[b:]), nil
}

func schema(data []byte) ([]byte, error) {
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	properties, ok := value["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("propertiesがありません")
	}
	for key, description := range map[string]string{"level": tierpolicy.ConsultationRules, "reason": tierpolicy.ReasonRules} {
		property, ok := properties[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("properties.%sがありません", key)
		}
		old, ok := property["description"].(string)
		if !ok {
			return nil, fmt.Errorf("properties.%s.descriptionが文字列ではありません", key)
		}
		oldJSON, _ := json.Marshal(old)
		nextJSON, _ := json.Marshal(description)
		if bytes.Count(data, oldJSON) != 1 {
			return nil, fmt.Errorf("descriptionを一意に特定できません: %s", key)
		}
		data = bytes.Replace(data, oldJSON, nextJSON, 1)
	}
	return data, nil
}

func run(root string, check bool) error {
	blocks := map[string]string{
		"skills/issue-desk-operations/SKILL.md":                   tierpolicy.SelectionRules + "\n\n相談と実装の判定基準・引継ぎは [難易度と分割](refs/tier-and-splitting.md)、確定前は [相談の確定](../issue-desk-finalization/SKILL.md) を読む。",
		"skills/issue-desk-operations/refs/tier-and-splitting.md": "## 相談の内容判定\n\n" + tierpolicy.ConsultationRules + "\n\n" + tierpolicy.ReasonRules + "\n\n" + tierpolicy.SelectionRules + "\n\n## 実装の機械判定\n\n" + tierpolicy.ImplementationRules() + "\n\n## 資料の引継ぎ\n\n" + tierpolicy.HandoffRules + "\n\n## 分割と確定\n\n" + tierpolicy.SplittingRules + "\n\n" + tierpolicy.FinalizationRules,
		"skills/issue-intake/SKILL.md":                            tierpolicy.Guidance(),
		"skills/issue-desk-finalization/SKILL.md":                 tierpolicy.FinalizationRules + "\n\n## 資料と判断の根拠\n\n" + tierpolicy.HandoffRules + "\n\n## 二つの難易度\n\n" + tierpolicy.SelectionRules + "\n\n相談: " + tierpolicy.ConsultationRules + "\n\n" + tierpolicy.ImplementationRules() + "\n\n" + tierpolicy.SplittingRules + "\n\n起票・相談への回答は既存のissue-desk-operations Skillと受付APIの条件に従う。この点検は起票・PR・承認・状態変更の追加許可ではない。",
	}
	// These removed defaults must not reappear outside the generated block.
	retired := map[string][]string{
		"skills/issue-desk-operations/SKILL.md":                   {"通常は `light`"},
		"skills/issue-desk-operations/refs/tier-and-splitting.md": {"通常は Sonnet", "Sonnet（低または中）"},
		"skills/issue-intake/SKILL.md":                            {"Sonnetで始め相談ごとに見直す。"},
	}
	outputs := map[string][]byte{}
	paths := []string{"skills/issue-desk-operations/SKILL.md", "skills/issue-desk-operations/refs/tier-and-splitting.md", "skills/issue-intake/SKILL.md", "skills/issue-desk-finalization/SKILL.md", "api/schemas/tier.schema.json"}
	changed := []string{}
	for _, path := range paths {
		original, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return err
		}
		for _, old := range retired[path] {
			if bytes.Contains(original, []byte(old)) {
				return fmt.Errorf("%s: 独立した旧基準が残っています: %s", path, old)
			}
		}
		var out []byte
		if content, ok := blocks[path]; ok {
			out, err = section(original, content)
		} else {
			out, err = schema(original)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		outputs[path] = out
		if !bytes.Equal(original, out) {
			changed = append(changed, path)
		}
	}
	if check && len(changed) > 0 {
		return fmt.Errorf("難易度正本と不一致: %s", strings.Join(changed, ", "))
	}
	if !check {
		for _, path := range changed {
			if err := os.WriteFile(filepath.Join(root, path), outputs[path], 0644); err != nil {
				return err
			}
		}
	}
	return nil
}
func main() {
	root := flag.String("root", "..", "リポジトリのルート")
	check := flag.Bool("check", false, "不一致を検出し、ファイルを書き換えない")
	flag.Parse()
	if err := run(*root, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
