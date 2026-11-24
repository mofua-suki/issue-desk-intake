package proposals

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/consultations"
	"issue-desk-intake/server/internal/filing"
)

// 出荷する assignments.json の共通 models（実装の難易度の設定）を受付の設定値の形にする。
func shippedTierConfig(t *testing.T) Object {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "assignments.json"))
	if err != nil {
		t.Fatal(err)
	}
	assignments := Object{}
	if err := json.Unmarshal(raw, &assignments); err != nil {
		t.Fatal(err)
	}
	implementation := obj(obj(assignments["models"])["implementation"])
	if obj(implementation["tiers"]) == nil {
		t.Fatal("出荷設定に実装の難易度がありません")
	}
	return Object{"assignments": assignments, "implementation": implementation}
}

// PR の軽微な指摘への対応（後続案）は、起票と同じ規則で難易度が決まる。変更範囲と受入条件（テスト方法）が
// あれば低、範囲が無ければ中、light 無効の設定なら中。理由の文は Issue 本文と相談の記録にそのまま出る。
func TestFollowupTierFollowsFilingRule(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutate        func(Object, Object)
		level, reason string
	}{
		{"scope and tests", func(Object, Object) {}, "light", "変更範囲の宣言と受入条件あり"},
		{"no scope", func(_ Object, p Object) { p["changeScope"] = nil }, "standard", "変更範囲の宣言なし"},
		{"light disabled", func(c Object, _ Object) {
			obj(c["implementation"])["disableLightTier"] = true
		}, "standard", "変更範囲の宣言と受入条件あり（disableLightTier 設定で light 無効）"},
		{"five items", func(_ Object, p Object) {
			items := []any{}
			for range 5 {
				items = append(items, Object{"problem": "指摘", "expected": "修正", "test": "go test ./..."})
			}
			p["items"] = items
		}, "light", "変更範囲の宣言と受入条件あり"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p := shippedTierConfig(t), v3()
			tc.mutate(c, p)
			tier := followupTier(c, p)
			if tier["level"] != tc.level || tier["source"] != "desk" || tier["reason"] != tc.reason || str(tier["decidedAt"]) == "" {
				t.Fatalf("tier=%v", tier)
			}
			name := map[string]string{"light": "低", "standard": "中"}[tc.level]
			if got := filing.TierDescription(tier); got != " / 難易度: "+name+"（受付の判定: "+tc.reason+"）" {
				t.Fatalf("description=%q", got)
			}
		})
	}
	// 難易度の設定が無い構成では、起票と同じく難易度を決めない（目印も付けない）。
	if tier := followupTier(Object{}, v3()); tier != nil {
		t.Fatal(tier)
	}
}

// 起票本文の目印・ジョブ・相談の記録（Fields の implementationTier）に同じ難易度が入り、
// 呼び出し側が選んだモデルの難易度と食い違えば起票しない。
func TestPrepareFollowupRecordsDecidedTier(t *testing.T) {
	s, api, restart := adoptFixture(t)
	p := v3()
	api.comments[0] = proposalComment(p, 10)
	// 採用の許可設定は雛形のまま使い、実装の難易度の設定だけを出荷設定から足す。
	base, implementation := s.o.Config, obj(shippedTierConfig(t)["implementation"])
	config := Object{}
	s.o.Config = func() (Object, error) {
		c, err := base()
		if err != nil {
			return nil, err
		}
		for k, v := range c {
			config[k] = v
		}
		config["implementation"] = implementation
		return config, nil
	}
	s.o.Submit = func(context.Context, string, consultations.Record) (consultations.Record, error) { return nil, nil }
	mustScan(t, s)
	restart()
	d := savedDraft(t, s)
	level, err := s.FollowupTier(context.Background(), d)
	if err != nil || level != "light" {
		t.Fatal(level, err)
	}
	prepared, err := s.PrepareFollowup(context.Background(), d, Object{"provider": "codex", "model": "fixture-model", "effort": "medium", "tier": level}, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorded := obj(prepared.Fields["implementationTier"])
	if recorded["level"] != "light" || recorded["source"] != "desk" || recorded["reason"] != "変更範囲の宣言と受入条件あり" {
		t.Fatalf("記録: %v", prepared.Fields)
	}
	if prepared.Job.Tier == nil || *prepared.Job.Tier != "light" {
		t.Fatalf("ジョブの難易度: %v", prepared.Job.Tier)
	}
	for _, part := range []string{`"tier":"light"`, `"tierSource":"desk"`, "推論: medium / 難易度: 低（受付の判定: 変更範囲の宣言と受入条件あり）"} {
		if !strings.Contains(prepared.Body, part) {
			t.Fatalf("missing %q: %s", part, prepared.Body)
		}
	}
	if strings.Contains(prepared.Body, `"tier":"standard"`) {
		t.Fatal("固定の standard が残っています", prepared.Body)
	}
	if _, err := s.PrepareFollowup(context.Background(), d, Object{"provider": "codex", "model": "fixture-model", "effort": "medium", "tier": "standard"}, nil); err == nil {
		t.Fatal("選んだモデルの難易度と食い違う起票を通しました")
	}
}
