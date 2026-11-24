// Package workplan keeps executable acceptance conditions separate from their prose.
package workplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Requirement struct {
	ID           string   `json:"id"`
	Criterion    string   `json:"criterion"`
	Owner        string   `json:"owner"`
	Phase        string   `json:"phase"`
	Method       string   `json:"method"`
	Expected     string   `json:"expected"`
	EvidenceKind string   `json:"evidenceKind"`
	Capabilities []string `json:"capabilities"`
	Inputs       []string `json:"inputs"`
	Children     []int    `json:"children"`
}
type Plan struct {
	Version      int           `json:"version"`
	Requirements []Requirement `json:"requirements"`
}
type Evidence struct {
	Requirement string `json:"requirement"`
	Generation  int64  `json:"generation"`
	RecordID    string `json:"recordID"`
	PlanSHA     string `json:"planSHA"`
	Head        string `json:"head"`
	Status      string `json:"status"`
	Source      string `json:"source"`
	Detail      string `json:"detail"`
}

// DecodeEvidence rejects the entire collection on a malformed item. A prefix
// decoded before an error must never be used as proof.
func DecodeEvidence(value any) ([]Evidence, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var evidence []Evidence
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return nil, err
	}
	return evidence, nil
}

type Check struct {
	Method       string   `json:"method"`
	Expected     string   `json:"expected"`
	EvidenceKind string   `json:"evidenceKind"`
	Capabilities []string `json:"capabilities"`
	Inputs       []string `json:"inputs"`
	ID           string   `json:"id"`
	Criterion    string   `json:"criterion"`
	Owner        string   `json:"owner"`
	Phase        string   `json:"phase"`
	Status       string   `json:"status"`
	Reason       string   `json:"reason"`
}

// Parse accepts absent legacy plans, but never converts malformed new data to legacy.
func Parse(value any) (*Plan, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 64*1024 {
		return nil, errors.New("実行計画を読めません")
	}
	var p Plan
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || p.Version != 1 || len(p.Requirements) == 0 || len(p.Requirements) > 30 {
		return nil, errors.New("実行計画は版1・受入条件1〜30件です")
	}
	ids := map[string]bool{}
	for _, r := range p.Requirements {
		if err := checkRequirement(r, ids); err != nil {
			return nil, err
		}
	}
	p.fillEmptyLists()
	return &p, nil
}

// checkRequirement validates one acceptance condition and records its id in ids.
func checkRequirement(r Requirement, ids map[string]bool) error {
	if strings.TrimSpace(r.ID) == "" || len(r.ID) > 100 || ids[r.ID] || strings.TrimSpace(r.Criterion) == "" || len(r.Criterion) > 8000 || strings.TrimSpace(r.Method) == "" || len(r.Method) > 4000 || strings.TrimSpace(r.Expected) == "" || len(r.Expected) > 4000 {
		return errors.New("受入条件の識別子・確認手段・期待結果が不正です")
	}
	if !contains([]string{"fixed_test", "report_comment", "external_check"}, r.EvidenceKind) {
		return errors.New("受入条件の証拠型が不正です")
	}
	if r.EvidenceKind == "report_comment" && (r.Owner != "reporter" || r.Phase != "report") || r.EvidenceKind == "external_check" && r.Owner != "controller" {
		return errors.New("確認手段と担当が一致しません")
	}
	if r.EvidenceKind == "fixed_test" && (r.Phase == "deployment" || r.Phase == "report") {
		return errors.New("固定検証で配置・報告の成功を代替できません")
	}
	ids[r.ID] = true
	if !contains([]string{"implementer", "controller", "reporter"}, r.Owner) || !contains([]string{"implementation", "verification", "deployment", "report"}, r.Phase) {
		return errors.New("実行主体か工程が不正です")
	}
	if r.Owner == "reporter" && r.Phase != "report" {
		return errors.New("報告訂正はreport工程です")
	}
	if len(r.Capabilities) > 20 || len(r.Inputs) > 30 || len(r.Children) > 25 {
		return errors.New("必要能力・資料・対象の子が多すぎます")
	}
	for _, c := range r.Capabilities {
		if !contains([]string{"race", "gcc", "cgo", "docker", "browser", "systemd", "windows", "realDevice", "liveMeasurement", "history", "network", "otherRepository"}, c) {
			return fmt.Errorf("未知の必要能力: %s", c)
		}
		if (r.Owner == "implementer" || r.EvidenceKind == "fixed_test") && contains([]string{"realDevice", "liveMeasurement", "history", "otherRepository"}, c) {
			return fmt.Errorf("%sは実装担当へ渡せません。資料を用意するコントローラーの確認へ分けてください", c)
		}
	}
	for _, v := range r.Inputs {
		if strings.TrimSpace(v) == "" || len(v) > 2000 {
			return errors.New("必要資料の出典が不正です")
		}
	}
	seen := map[int]bool{}
	for _, n := range r.Children {
		if n < 1 || n > 25 || seen[n] {
			return errors.New("対象の子が不正です")
		}
		seen[n] = true
	}
	return nil
}

// fillEmptyLists stores absent lists as empty ones.
func (p *Plan) fillEmptyLists() {
	for i := range p.Requirements {
		if p.Requirements[i].Capabilities == nil {
			p.Requirements[i].Capabilities = []string{}
		}
		if p.Requirements[i].Inputs == nil {
			p.Requirements[i].Inputs = []string{}
		}
		if p.Requirements[i].Children == nil {
			p.Requirements[i].Children = []int{}
		}
	}
}
func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
func (p *Plan) Fingerprint() string {
	canonical := *p
	canonical.Requirements = append([]Requirement{}, p.Requirements...)
	for i := range canonical.Requirements {
		r := &canonical.Requirements[i]
		if r.Capabilities == nil {
			r.Capabilities = []string{}
		}
		if r.Inputs == nil {
			r.Inputs = []string{}
		}
		if r.Children == nil {
			r.Children = []int{}
		}
	}
	raw, _ := json.Marshal(canonical)
	s := sha256.Sum256(raw)
	return hex.EncodeToString(s[:])
}
func (p *Plan) Record() map[string]any {
	raw, _ := json.Marshal(p)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
func (p *Plan) ForChild(index int) *Plan {
	out := &Plan{Version: 1, Requirements: []Requirement{}}
	for _, r := range p.Requirements {
		applies := len(r.Children) == 0
		for _, n := range r.Children {
			applies = applies || n == index
		}
		if applies {
			out.Requirements = append(out.Requirements, r)
		}
	}
	return out
}

// Checks does not treat a model's completed status as proof. Evidence must match
// the exact plan, head and the owner that actually performed that check.
func (p *Plan) Checks(head string, evidence []Evidence, generation ...int64) []Check {
	out := make([]Check, 0, len(p.Requirements))
	fingerprint := p.Fingerprint()
	for _, r := range p.Requirements {
		c := Check{Method: r.Method, Expected: r.Expected, EvidenceKind: r.EvidenceKind, Capabilities: r.Capabilities, Inputs: r.Inputs, ID: r.ID, Criterion: r.Criterion, Owner: r.Owner, Phase: r.Phase, Status: "pending", Reason: "対象版の実行証拠がありません"}
		for _, e := range evidence {
			if (len(generation) == 0 || e.Generation == generation[0]) && e.Requirement == r.ID && e.PlanSHA == fingerprint && len(head) == 40 && e.Head == head && e.Source == r.Owner && strings.TrimSpace(e.Detail) != "" {
				if e.Status == "passed" {
					c.Status = "passed"
					c.Reason = ""
				} else if e.Status == "failed" {
					c.Status = "failed"
					c.Reason = e.Detail
				}
				break
			}
		}
		out = append(out, c)
	}
	return out
}

// Section preserves every condition, including controller work, without asking
// the implementation AI to execute operations outside its environment.
func (p *Plan) Section() string {
	var b strings.Builder
	b.WriteString("## 受入条件の実行分担\n")
	for _, r := range p.Requirements {
		fmt.Fprintf(&b, "\n- %s: %s\n  担当=%s / 工程=%s\n  確認=%s → 期待=%s\n", r.ID, r.Criterion, r.Owner, r.Phase, r.Method, r.Expected)
		if len(r.Inputs) > 0 {
			fmt.Fprintf(&b, "  必要資料=%s\n", strings.Join(r.Inputs, "、"))
		}
	}
	return b.String()
}
