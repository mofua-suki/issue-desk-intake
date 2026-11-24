package filing

import (
	"context"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestTierDisableLightAtFiling(t *testing.T) {
	for _, tc := range []struct {
		name          string
		setting       any
		requested     string
		stall, heavy  bool
		level, source string
	}{
		{"absent", nil, "", false, false, "light", "desk"},
		{"false", false, "", false, false, "light", "desk"},
		{"true", true, "", false, false, "standard", "desk"},
		{"requested", true, "light", false, true, "light", "user"},
		{"stall", true, "", true, true, "standard", "desk"},
		{"heavy", true, "", false, true, "heavy", "desk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := configFixture()
			impl := row(c.Config["implementation"])
			if tc.setting != nil {
				impl["disableLightTier"] = tc.setting
			}
			d := draftFixture()
			d["body"] = "## 受入条件\n- 一つ\n- 二つ\n- 三つ\n- 四つ"
			d["estimate"] = Record{"lines": 50, "files": 1}
			if tc.requested != "" {
				d["requestedImplementationTier"] = tc.requested
			}
			if tc.stall {
				d["stallFix"] = true
			}
			if tc.heavy {
				d["crossProject"] = true
			}
			if tc.stall {
				tier := decideTier(c, d)
				if tier["level"] != tc.level || tier["source"] != tc.source {
					t.Fatal(tier)
				}
				return // Prepare は自動監視の新規起票を停止している。
			}
			p, err := Prepare(context.Background(), c, d)
			if err != nil {
				t.Fatal(err)
			}
			tier := row(p.Fields["implementationTier"])
			if tier["level"] != tc.level || tier["source"] != tc.source {
				t.Fatal(tier)
			}
			if tc.name == "true" && (!strings.Contains(fleet.Text(tier["reason"]), "無効") || !strings.Contains(p.Body, "disableLightTier")) {
				t.Fatal(p.Body)
			}
			if p.Job.Tier == nil || *p.Job.Tier != tc.level {
				t.Fatal(p.Job.Tier)
			}
		})
	}
}

func TestTierCountScopeAndEstimateIndependent(t *testing.T) {
	estimates := []any{nil, Record{"lines": 150, "files": 5}, Record{"lines": 151, "files": 5}, Record{"lines": 150, "files": 6}, Record{"lines": 3000, "files": 100}}
	for _, count := range []int{1, 3, 4, 10} {
		for _, n := range []int{1, 5, 6, 40} {
			for i, estimate := range estimates {
				for _, setting := range []any{nil, false} {
					t.Run(fmt.Sprintf("conditions=%d/paths=%d/estimate=%d/setting=%v", count, n, i, setting), func(t *testing.T) {
						c, d := configFixture(), draftFixture()
						if setting != nil {
							row(c.Config["implementation"])["disableLightTier"] = setting
						}
						paths := make([]any, n)
						for j := range paths {
							paths[j] = fmt.Sprintf("src/file%d.go", j)
						}
						d["changeScope"] = Record{"known": true, "paths": paths}
						d["body"] = "## 受入条件\n" + strings.Repeat("- 確認: go test ./... → 期待: 終了コード0\n", count)
						d["estimate"] = estimate
						p, err := Prepare(context.Background(), c, d)
						if err != nil {
							t.Fatal(err)
						}
						tier := row(p.Fields["implementationTier"])
						reason := fleet.Text(tier["reason"])
						if tier["level"] != "light" || p.Job.Tier == nil || *p.Job.Tier != "light" || !strings.Contains(p.Body, `"tier":"light"`) || !strings.Contains(p.Body, "難易度: 低（受付の判定: "+reason+"）") {
							t.Fatalf("tier=%v job=%v body=%s", tier, p.Job.Tier, p.Body)
						}
						for _, old := range []string{"5件以下", "3件以下", "見込み"} {
							if strings.Contains(reason, old) {
								t.Fatal(reason)
							}
						}
					})
				}
			}
		}
	}
}

func TestTierHeavyAndPriorityPreserved(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutate        func(Record)
		level, source string
	}{
		{"unknown", func(d Record) { d["changeScope"] = Record{"known": false} }, "standard", "desk"},
		{"cross project", func(d Record) { d["crossProject"] = true }, "heavy", "desk"},
		{"reason", func(d Record) { d["heavyReason"] = "分割できない" }, "heavy", "desk"},
		{"explicit", func(d Record) {
			d["requestedImplementationTier"] = "light"
			d["stallFix"] = true
			d["crossProject"] = true
		}, "light", "user"},
		{"stall", func(d Record) { d["stallFix"] = true; d["crossProject"] = true }, "standard", "desk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := draftFixture()
			tc.mutate(d)
			tier := decideTier(configFixture(), d)
			if tier["level"] != tc.level || tier["source"] != tc.source {
				t.Fatal(tier)
			}
			if tc.name == "unknown" {
				if _, err := Prepare(context.Background(), configFixture(), d); err == nil {
					t.Fatal("unknown scope must be rejected")
				}
			}
		})
	}
}

func TestTierLightPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(Record)
		level  string
	}{
		{"no scope", func(d Record) { delete(d, "changeScope") }, "standard"},
		{"no section", func(d Record) { d["body"] = "## 目的\n説明" }, "standard"},
		{"empty", func(d Record) { d["body"] = "## 受入条件\n説明" }, "standard"},
		{"fenced", func(d Record) { d["body"] = "## 受入条件\n```\n- 確認: go test → 期待: PASS\n```" }, "standard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := draftFixture()
			tc.mutate(d)
			tier := decideTier(configFixture(), d)
			if tc.name == "no scope" {
				if tier["level"] != tc.level {
					t.Fatal(tier)
				}
				if _, err := Prepare(context.Background(), configFixture(), d); err == nil {
					t.Fatal("missing scope must be rejected")
				}
				return
			}
			p, err := Prepare(context.Background(), configFixture(), d)
			if err != nil {
				t.Fatal(err)
			}
			if tier := row(p.Fields["implementationTier"]); tier["level"] != tc.level || p.Job.Tier == nil || *p.Job.Tier != tc.level {
				t.Fatal(tier, p.Job.Tier)
			}
		})
	}
	c, d := configFixture(), draftFixture()
	delete(row(c.Config["implementation"]), "tiers")
	p, err := Prepare(context.Background(), c, d)
	if err != nil {
		t.Fatal(err)
	}
	if row(p.Fields["implementationTier"]) != nil || p.Job.Tier != nil || strings.Contains(p.Body, `"tier":`) || strings.Contains(p.Body, "難易度:") {
		t.Fatal(p)
	}
}

// 範囲の宣言が不明なだけの依頼は中（standard）。高（heavy）は複数プロジェクトと分けられない理由だけで、
// 不明が重なっても高の理由の文に「不明」を混ぜない（理由の文は完全一致で確かめる）。理由の文は Issue 本文の難易度の行にそのまま出る。
func TestTierUnknownScopeAloneIsStandard(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mutate        func(Record)
		level, reason string
	}{
		{"unknown alone", func(Record) {}, "standard", "変更範囲の宣言が不明"},
		{"unknown with reason text", func(d Record) { d["changeScope"] = Record{"known": false, "reason": "探しても決められない"} }, "standard", "変更範囲の宣言が不明"},
		{"unreadable declaration", func(d Record) { d["changeScope"] = Record{"paths": []any{"src/a.go"}} }, "standard", "変更範囲の宣言が不明"},
		{"unknown without acceptance", func(d Record) { d["body"] = "## 目的\n説明" }, "standard", "変更範囲の宣言が不明・受入条件の節なし"},
		{"unknown and cross project", func(d Record) { d["crossProject"] = true }, "heavy", "複数プロジェクトの案"},
		{"unknown and heavy reason", func(d Record) { d["heavyReason"] = "分けると移行が壊れる" }, "heavy", "分けられない理由: 分けると移行が壊れる"},
		{"unknown and requested", func(d Record) { d["requestedImplementationTier"] = "heavy" }, "heavy", "依頼者の指定"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, d := configFixture(), draftFixture()
			d["changeScope"] = Record{"known": false}
			tc.mutate(d)
			tier := decideTier(c, d)
			if tier["level"] != tc.level || tier["reason"] != tc.reason {
				t.Fatalf("tier=%v", tier)
			}
			if got := tierDescription(tier); tc.level == "standard" && got != " / 難易度: 中（受付の判定: "+tc.reason+"）" {
				t.Fatalf("description=%q", got)
			}
		})
	}
}
