// Package completionplan validates an auditable execution plan. Its records never
// authorize a protected action or replace the action's own approval checks.
package completionplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type Step struct {
	Phase        string `json:"phase"`
	Owner        string `json:"owner"`
	Prerequisite string `json:"prerequisite"`
	Evidence     string `json:"evidence"`
}
type Evidence struct {
	MessageID string `json:"messageId"`
	Quote     string `json:"quote"`
	At        string `json:"at"`
	Source    string `json:"source,omitempty"`
}
type Approval struct {
	Category  string    `json:"category"`
	ID        string    `json:"id"`
	Phase     string    `json:"phase"`
	Target    string    `json:"target"`
	Operation string    `json:"operation"`
	Condition string    `json:"condition"`
	Evidence  *Evidence `json:"evidence,omitempty"`
	Gate      *string   `json:"gate,omitempty"`
	Children  []int     `json:"children,omitempty"`
}
type Plan struct {
	Version   int        `json:"version"`
	Basis     string     `json:"basis"`
	Steps     []Step     `json:"steps"`
	Approvals []Approval `json:"approvals"`
}

func Parse(value any) (Plan, error) {
	var p Plan
	raw, err := json.Marshal(value)
	if err != nil {
		return p, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&p); err != nil {
		return p, fmt.Errorf("完了計画の形式が不正です: %w", err)
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return p, fmt.Errorf("完了計画を読み切れません")
	}
	if p.Version != 1 || !meaningful(p.Basis) || len(p.Steps) != 4 || p.Approvals == nil || len(p.Approvals) > 20 {
		return p, fmt.Errorf("完了計画には版1・判断の前提・実装/検証/マージ/配置の4工程・承認一覧が必要です")
	}
	if err = checkSteps(p.Steps); err != nil {
		return p, err
	}
	if err = checkApprovals(p.Approvals); err != nil {
		return p, err
	}
	return p, nil
}

// checkSteps requires each of the four phases once, with its owner, prerequisite and evidence.
func checkSteps(steps []Step) error {
	seen := map[string]bool{}
	for _, step := range steps {
		if !map[string]bool{"implementation": true, "verification": true, "merge": true, "deployment": true}[step.Phase] || seen[step.Phase] || !meaningful(step.Owner) || !meaningful(step.Prerequisite) || !meaningful(step.Evidence) {
			return fmt.Errorf("完了計画の工程には固有の段階・担当・前提・確認方法が必要です")
		}
		seen[step.Phase] = true
	}
	return nil
}

// checkApprovals validates each approval's id, category, phase, gate and children.
func checkApprovals(approvals []Approval) error {
	seen := map[string]bool{}
	for _, a := range approvals {
		if CategoryLabel(a.Category) == "" || !meaningful(a.ID) || seen[a.ID] || (a.Phase != "before_implementation" && a.Phase != "after_diff") || !meaningful(a.Target) || !meaningful(a.Operation) || !meaningful(a.Condition) {
			return fmt.Errorf("承認事項には固有ID・判断分類・時点・対象・操作・条件が必要です")
		}
		if a.Gate != nil && (a.Phase != "after_diff" || !(*a.Gate == "permission" && a.Category == "permission" || *a.Gate == "deletion" && a.Category == "destructive")) {
			return fmt.Errorf("完了計画の形式が不正です: gateはafter_diffのpermission分類にpermission、destructive分類にdeletionだけを指定できます")
		}
		children := map[int]bool{}
		for _, index := range a.Children {
			if index < 1 || index > 25 || children[index] {
				return fmt.Errorf("完了計画の形式が不正です: childrenには1〜25の重複しない分割案の番号が必要です")
			}
			children[index] = true
		}
		seen[a.ID] = true
	}
	return nil
}

// AppliesTo keeps approvals with no child restriction or an unknown child index.
func (a Approval) AppliesTo(index int) bool {
	if len(a.Children) == 0 || index < 1 {
		return true
	}
	for _, child := range a.Children {
		if child == index {
			return true
		}
	}
	return false
}

func meaningful(s string) bool {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "なし", "無し", "該当なし", "none", "n/a", "-":
		return false
	}
	return len(s) <= 8000
}
func (p Plan) Fingerprint() string {
	copy := p
	copy.Approvals = append([]Approval(nil), p.Approvals...)
	for i := range copy.Approvals {
		copy.Approvals[i].Evidence = nil
	}
	b, _ := json.Marshal(copy)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (p Plan) Record() map[string]any {
	b, _ := json.Marshal(p)
	var r map[string]any
	_ = json.Unmarshal(b, &r)
	return r
}
func (p Plan) Section() string {
	var b strings.Builder
	b.WriteString("## 完了までの計画と承認\n判断の前提: " + p.Basis + "\n")
	names := map[string]string{"implementation": "実装", "verification": "検証", "merge": "マージ", "deployment": "配置"}
	for _, s := range p.Steps {
		fmt.Fprintf(&b, "- %s: 担当=%s / 前提=%s / 確認=%s\n", names[s.Phase], s.Owner, s.Prerequisite, s.Evidence)
	}
	if len(p.Approvals) == 0 {
		b.WriteString("- 予見した追加承認事項: 現在の対象と前提では追加承認が必要な操作を予定しません。\n")
	}
	for _, a := range p.Approvals {
		timing := "実装前に明示確認"
		if a.Phase == "after_diff" {
			timing = "具体的な差分・対象SHAが確定した後に最終承認"
		}
		if !NeedsUser(a.Category) {
			timing = "利用者の回答を待たずに進め、結果を報告"
		}
		if a.Gate != nil {
			gateName := "権限保護"
			if *a.Gate == "deletion" {
				gateName = "削除承認"
			}
			timing = "既存の" + gateName + "ゲートに委ね、計画上の最終承認は別途求めない"
		}
		fmt.Fprintf(&b, "- 承認 %s（%s / %s）: 対象=%s / 操作=%s / 条件=%s\n", a.ID, timing, CategoryLabel(a.Category), a.Target, a.Operation, a.Condition)
		if len(a.Children) > 0 {
			children := make([]string, len(a.Children))
			for i, child := range a.Children {
				children[i] = fmt.Sprint(child)
			}
			fmt.Fprintf(&b, "  対象の子: %s\n", strings.Join(children, ", "))
		}
	}
	b.WriteString("この計画の了承記録は個別操作の認可を代替しません。承認事項の対象・操作・条件が変わった場合は再確認し、既存の削除・権限・対象SHAの承認を維持します。")
	return b.String()
}

func CategoryLabel(category string) string {
	return map[string]string{"permission": "新しい権限", "destructive": "破壊的な操作", "irreversible": "取り返しのつかない操作", "scope": "範囲外の変更", "basis": "判断の根拠の変化"}[category]
}
