package filingcheck

import (
	"database/sql"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/jobs"
	"strings"
	"testing"
)

func TestSplitSiblingDependencyAndUnknownState(t *testing.T) {
	d := Record{"id": "two", "parentSplit": "parent", "project": "owner/repo", "dependencies": []any{7, 8, Record{"repo": "other/repo", "number": 7}}}
	family := []Record{{"id": "one", "parentSplit": "parent", "issue": Record{"project": "owner/repo", "number": 7}}, {"id": "three", "parentSplit": "parent", "body": "未起票本文"}}
	target := Record{"dependsOnIssues": []any{Record{"source": "gitea", "number": 7}}}
	mergeSiblingDependencies(target, d, family)
	if len(fleet.Elements(target["dependsOnIssues"])) != 1 {
		t.Fatalf("dependencies: %v", target)
	}
	target["dependsOnIssues"] = []any{}
	mergeSiblingDependencies(target, d, family)
	if len(fleet.Elements(target["dependsOnIssues"])) != 1 || fleet.WholeNumber(obj(fleet.Elements(target["dependsOnIssues"])[0])["number"]) != 7 {
		t.Fatalf("dependencies: %v", target)
	}
	s := &Service{}
	rows := s.splitSiblings(d, append(family, Record{"id": "parent", "splitChildren": []any{"one", "two", "three"}}), nil)
	if len(rows) != 2 || obj(rows[0])["state"] != nil || obj(rows[0])["commonNumber"] != nil || obj(rows[1])["body"] != "未起票本文" {
		t.Fatalf("rows: %v", rows)
	}
}
func TestSplitSiblingPromptRules(t *testing.T) {
	for _, rule := range []string{"分割案NはdecisionContext.splitSiblingsの対応表で照合", "特定できる依存を特定不能としません", "取得失敗・未確認を満たされた依存としない", "重複"} {
		if !strings.Contains(instructions, rule) {
			t.Fatalf("missing %s", rule)
		}
	}
}

func TestLongSiblingBodiesAreOmittedAndRecorded(t *testing.T) {
	rows := []any{}
	for i := 0; i < 25; i++ {
		rows = append(rows, Record{"consultationId": fmt.Sprintf("s%d", i), "proposalNumber": i + 1, "body": strings.Repeat("本文", 4000)})
	}
	original := Record{"target": Record{"body": "対象", "decisionContext": Record{"splitSiblings": rows}}}
	s := &Service{}
	if s.inputSize(original) <= inputLimit {
		t.Fatal("fixture does not exceed input limit")
	}
	out := reduceReferences(original)
	if size := s.inputSize(out); size > inputLimit {
		t.Fatalf("input size %d", size)
	}
	for _, item := range fleet.Elements(obj(obj(out["target"])["decisionContext"])["splitSiblings"]) {
		row := obj(item)
		if row["body"] != nil || row["bodyOmitted"] != true {
			t.Fatalf("sibling: %v", row)
		}
	}
	omissions := inputOmissions(out)
	if len(omissions) != 1 || obj(omissions[0])["kind"] != "未起票兄弟本文" || obj(omissions[0])["reason"] != "入力上限" || len(fleet.Elements(obj(omissions[0])["candidateIds"])) != 25 {
		t.Fatal(omissions)
	}
	if obj(rows[0])["body"] == nil {
		t.Fatal("original body changed")
	}
}

func TestCheckFiledSiblingDependencyWithoutCandidate(t *testing.T) {
	f := newSiblingCheckFixture(t)
	// Exercise the saved plan alone, without the generated declaration or bound numbers.
	f.d["body"] = "先行契約を利用する"
	f.persist(t)
	if len(fleet.Elements(f.d["dependsOnIssues"])) != 0 || len(fleet.Elements(f.d["dependencies"])) != 0 {
		t.Fatal(f.d)
	}
	f.answer = func(m Record) map[string]any {
		if candidates(m)["request:483"] != nil || candidates(m)["job:483"] != nil || candidates(m)["request:484"] != nil || candidates(m)["job:484"] != nil {
			t.Fatal("family became duplicate candidates")
		}
		deps, states := fleet.Elements(obj(m["target"])["dependsOnIssues"]), fleet.Elements(m["explicitDependencies"])
		if len(deps) != 1 || fleet.WholeNumber(obj(deps[0])["number"]) != 483 || len(states) != 1 || fleet.WholeNumber(obj(states[0])["number"]) != 483 || obj(states[0])["state"] != "open" || obj(states[0])["jobStatus"] != "queued" || obj(states[0])["satisfied"] != false {
			t.Fatalf("dependency material: %v", m)
		}
		siblings := fleet.Elements(obj(obj(m["target"])["decisionContext"])["splitSiblings"])
		if len(siblings) != 2 || fleet.WholeNumber(obj(siblings[0])["proposalNumber"]) != 1 || fleet.WholeNumber(obj(siblings[0])["giteaNumber"]) != 483 || fleet.WholeNumber(obj(siblings[0])["commonNumber"]) != 5435 {
			t.Fatal(siblings)
		}
		out := allowed()
		out["dependsOnIssues"] = []any{Record{"source": "gitea", "number": 483}}
		return out
	}
	assertSiblingCheckAllowed(t, f, 483)
	if f.statusReads != 1 {
		t.Fatalf("status reads=%d", f.statusReads)
	}
}

func TestCheckFiledSiblingDependencyDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"generated", "", false},
		{"sentence", "分割案1に依存する。", false},
		{"generic list", "依存先: 分割案1", false},
		{"requester list", "依頼者の指定による依存: 分割案1", true},
		{"title mentioning another sibling", "依頼者の指定による依存: 分割案1（分割案2を調査）", true},
		{"listed only", "分割案1と分割案2が兄弟です。", false},
		{"negative", "分割案1に依存しない。", false},
		{"negative list", "依存: 分割案1ではない", false},
		{"fenced example", "```text\n依頼者の指定による依存: 分割案1\n```", false},
		{"nested fence example", "````text\n```\n依頼者の指定による依存: 分割案1\n```\n````", false},
		{"quote", "> 依頼者の指定による依存: 分割案1", false},
		{"reference", "対象本文\n<!-- issue-desk-reference:start -->\n<details>\n<summary>受付の経緯・根拠（参照）</summary>\n依頼者の指定による依存: 分割案1\n</details>\n<!-- issue-desk-reference:end -->", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSiblingCheckFixture(t)
			obj(fleet.Elements(f.parent["splitIssues"])[2])["dependsOn"] = []any{}
			if tc.body != "" {
				f.d["body"] = tc.body
			}
			// A declared dependency must also reach the missing-file overlap gate.
			f.sibling["changeScope"] = f.d["changeScope"]
			if !tc.want {
				f.sibling["changeScope"] = nil
			}
			f.persist(t)
			f.answer = func(m Record) map[string]any {
				n := len(fleet.Elements(obj(m["target"])["dependsOnIssues"]))
				if n != 0 && !tc.want || n != 1 && tc.want {
					t.Fatalf("inferred dependencies: %v", m)
				}
				return allowed()
			}
			if tc.want {
				assertSiblingCheckAllowed(t, f, 483)
			} else {
				assertSiblingCheckAllowed(t, f)
			}
		})
	}
	t.Run("issues reference and saved dependencies", func(t *testing.T) {
		f := newSiblingCheckFixture(t)
		obj(fleet.Elements(f.parent["splitIssues"])[2])["dependsOn"] = []any{2}
		f.d["body"] = "本文"
		f.d["dependencies"] = []any{483}
		f.persist(t)
		assertSiblingCheckAllowed(t, f, 483, 484)
	})
}

func TestCheckContractFirstIgnoresBodyOnlyImplementationDependency(t *testing.T) {
	f := newSiblingCheckFixture(t)
	// The second proposal implements the contract; the target uses that contract.
	obj(fleet.Elements(f.parent["splitIssues"])[1])["dependsOn"] = []any{1}
	f.d["body"] = "契約を利用する。\n依存先: 分割案2"
	f.persist(t)
	f.answer = func(m Record) map[string]any {
		deps := fleet.Elements(obj(m["target"])["dependsOnIssues"])
		if len(deps) != 1 || fleet.WholeNumber(obj(deps[0])["number"]) != 483 {
			t.Fatalf("dependencies: %v", deps)
		}
		return allowed()
	}
	assertSiblingCheckAllowed(t, f, 483)
}

func TestCheckFiledSiblingDependencyGuards(t *testing.T) {
	for _, tc := range []struct {
		name         string
		change       func(*siblingCheckFixture)
		answerNumber int64
	}{
		{"unfiled", func(f *siblingCheckFixture) { delete(f.sibling, "issue") }, 483},
		{"other project reference", func(f *siblingCheckFixture) { obj(f.sibling["issue"])["project"] = "other/repo" }, 483},
		{"other project child", func(f *siblingCheckFixture) { f.sibling["project"] = "other/repo" }, 483},
		{"other project proposal", func(f *siblingCheckFixture) {
			obj(fleet.Elements(f.parent["splitIssues"])[0])["project"] = "other/repo"
		}, 483},
		{"wrong parent", func(f *siblingCheckFixture) { f.sibling["parentSplit"] = "another" }, 483},
		{"sibling outside mapping", func(f *siblingCheckFixture) { fleet.Elements(f.parent["splitChildren"])[0] = "missing" }, 483},
		{"target outside mapping", func(f *siblingCheckFixture) { fleet.Elements(f.parent["splitChildren"])[2] = "missing" }, 483},
		{"ambiguous mapping", func(f *siblingCheckFixture) { fleet.Elements(f.parent["splitChildren"])[1] = "one" }, 483},
		{"number outside mapping", func(f *siblingCheckFixture) {
			obj(fleet.Elements(f.parent["splitIssues"])[2])["dependsOn"] = []any{999}
			f.d["body"] = "この案の依存: 分割案999"
		}, 999},
		{"common number as proposal", func(f *siblingCheckFixture) {
			obj(fleet.Elements(f.parent["splitIssues"])[2])["dependsOn"] = []any{5435}
			f.d["body"] = "共通番号5435\nこの案の依存: 分割案5435"
		}, 5435},
		{"common number as gitea", func(f *siblingCheckFixture) {
			obj(fleet.Elements(f.parent["splitIssues"])[2])["dependsOn"] = []any{}
			f.d["body"] = "共通番号5435に依存する"
		}, 5435},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSiblingCheckFixture(t)
			tc.change(f)
			// Keep excluded-family guards independent of normal request proof.
			f.listing = []any{f.issues[900]}
			f.persist(t)
			f.answer = func(m Record) map[string]any {
				if len(fleet.Elements(obj(m["target"])["dependsOnIssues"])) != 0 {
					t.Fatalf("unproven sibling adopted: %v", m)
				}
				out := allowed()
				out["dependsOnIssues"] = []any{Record{"source": "gitea", "number": tc.answerNumber}}
				return out
			}
			err := f.s.Check(t.Context(), f.d)
			if err == nil || !strings.Contains(err.Error(), "今回の比較資料") || len(f.sent) != 1 {
				t.Fatalf("err=%v sent=%d", err, len(f.sent))
			}
			saved := f.saved(t)
			if len(fleet.Elements(saved["dependsOnIssues"])) != 0 || obj(saved["filingCheckCalls"])["outcome"] != "invalid_answer" {
				t.Fatal(saved)
			}
		})
	}
}

func TestCheckFiledSiblingDependencyState(t *testing.T) {
	for _, tc := range []struct {
		name, state, job                         string
		pr, merged, fail, noJob, satisfied, stop bool
	}{
		{name: "open queued", state: "open", job: "queued"},
		{name: "open merged", state: "open", job: "merged", satisfied: true},
		{name: "closed job", state: "closed", job: "closed", satisfied: true},
		{name: "closed queued", state: "closed", job: "queued"},
		{name: "merged pr", state: "closed", pr: true, merged: true, satisfied: true},
		{name: "unmerged pr", state: "closed", pr: true},
		{name: "fetch failure", state: "open", fail: true, stop: true},
		{name: "no job", state: "open", noJob: true, stop: true},
		{name: "unknown state", state: "unknown", job: "merged", stop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSiblingCheckFixture(t)
			f.issues[483]["state"] = tc.state
			f.failIssue = tc.fail
			if tc.pr {
				f.issues[483]["pull_request"] = Record{}
				f.pr = Record{"state": "closed", "merged": tc.merged}
			}
			if tc.noJob {
				if _, err := f.jobDB.ExecContext(t.Context(), "DELETE FROM jobs WHERE issue=483"); err != nil {
					t.Fatal(err)
				}
			} else if tc.job != "" {
				if _, err := f.jobDB.ExecContext(t.Context(), "UPDATE jobs SET status=$1 WHERE issue=483", tc.job); err != nil {
					t.Fatal(err)
				}
			}
			f.answer = func(m Record) map[string]any {
				states := fleet.Elements(m["explicitDependencies"])
				if len(states) != 1 || obj(states[0])["satisfied"] != tc.satisfied || obj(states[0])["reason"] != nil {
					t.Fatalf("states=%v", states)
				}
				return allowed()
			}
			if tc.stop {
				err := f.s.Check(t.Context(), f.d)
				if err == nil || len(f.sent) != 0 || len(fleet.Elements(f.saved(t)["dependsOnIssues"])) != 0 {
					t.Fatalf("err=%v calls=%d", err, len(f.sent))
				}
				if tc.noJob && !strings.Contains(err.Error(), "ジョブがありません") || tc.state == "unknown" && !strings.Contains(err.Error(), "状態が未確認") {
					t.Fatal(err)
				}
			} else {
				assertSiblingCheckAllowed(t, f, 483)
			}
		})
	}
}

func TestCheckNonSiblingDependencyProof(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		number   int64
		want     bool
	}{
		{"request match", "request:900", 900, true},
		{"request mismatch", "request:900", 5435, false},
		{"job match", "job:901", 901, true},
		{"job mismatch", "job:901", 900, false},
		{"joined job match", "job:900", 900, true},
		{"joined job mismatch", "job:900", 901, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSiblingCheckFixture(t)
			f.d["dependsOnIssues"] = []any{Record{"source": "gitea", "number": 484}}
			for _, n := range []int64{900, 901} {
				if _, err := f.s.options.Jobs.Create(t.Context(), jobs.NewJob{Repo: "owner/repo", Issue: n, Title: "通常候補", Scope: []string{"src/normal.go"}}); err != nil {
					t.Fatal(err)
				}
			}
			f.persist(t)
			f.answer = func(m Record) map[string]any {
				if candidates(m)[tc.id] == nil {
					t.Fatalf("candidate %s missing: %v", tc.id, m)
				}
				out := allowed()
				out["dependsOnIssues"] = []any{Record{"source": "gitea", "number": tc.number}}
				out["dependencyEvidence"] = []any{Record{"candidateId": tc.id, "number": tc.number}}
				return out
			}
			if tc.want {
				assertSiblingCheckAllowed(t, f, 484, 483, tc.number)
			} else {
				err := f.s.Check(t.Context(), f.d)
				if err == nil || !strings.Contains(err.Error(), "根拠IDが一致しません") {
					t.Fatal(err)
				}
				saved := f.saved(t)
				if len(fleet.Elements(saved["dependsOnIssues"])) != 1 || fleet.WholeNumber(obj(fleet.Elements(saved["dependsOnIssues"])[0])["number"]) != 484 {
					t.Fatal(saved)
				}
			}
		})
	}
}

func TestCheckFiledSiblingDependencyInputBudget(t *testing.T) {
	t.Run("reduce and record", func(t *testing.T) {
		f := newSiblingCheckFixture(t)
		// Full sibling bodies come from the same production snapshot as filed refs.
		for i := 0; i < 20; i++ {
			id := fmt.Sprintf("unfiled%d", i)
			row := Record{"id": id, "parentSplit": "parent", "project": "owner/repo", "body": strings.Repeat("未起票の本文", 4000), "status": "draft"}
			f.parent["splitChildren"] = append(fleet.Elements(f.parent["splitChildren"]), id)
			f.parent["splitIssues"] = append(fleet.Elements(f.parent["splitIssues"]), Record{"dependsOn": []any{}})
			if _, err := f.s.options.Records.Change(t.Context(), "drafts", id, func(*sql.Tx, Record) (Record, error) { return row, nil }); err != nil {
				t.Fatal(err)
			}
		}
		f.parent["messages"] = []any{Record{"role": "user", "text": strings.Repeat("祖先発言", 20000)}}
		f.persist(t)
		material, err := f.s.material(t.Context(), f.d)
		if err != nil {
			t.Fatal(err)
		}
		if f.s.inputSize(material) <= 160000 {
			t.Fatal("fixture does not exceed budget")
		}
		f.answer = func(m Record) map[string]any {
			siblings := fleet.Elements(obj(obj(m["target"])["decisionContext"])["splitSiblings"])
			for _, item := range siblings {
				row := obj(item)
				if strings.HasPrefix(fleet.Text(row["consultationId"]), "unfiled") && (row["body"] != nil || row["bodyOmitted"] != true) {
					t.Fatal(row)
				}
			}
			if len(fleet.Elements(m["explicitDependencies"])) != 1 || fleet.WholeNumber(obj(fleet.Elements(obj(m["target"])["dependsOnIssues"])[0])["number"]) != 483 {
				t.Fatal(m)
			}
			return allowed()
		}
		assertSiblingCheckAllowed(t, f, 483)
		checks := fleet.Elements(obj(f.saved(t)["filingCheckCalls"])["checks"])
		var siblingOmission, ancestorOmission bool
		for _, item := range fleet.Elements(obj(checks[0])["inputOmissions"]) {
			row := obj(item)
			if row["reason"] != "入力上限" {
				t.Fatal(row)
			}
			if row["kind"] == "未起票兄弟本文" && len(fleet.Elements(row["candidateIds"])) == 20 {
				siblingOmission = true
			}
			if row["kind"] == "祖先発言" && row["draftId"] == "parent" {
				ancestorOmission = true
			}
		}
		if !siblingOmission || !ancestorOmission {
			t.Fatal(checks)
		}
	})
	t.Run("protected body stops before answer", func(t *testing.T) {
		f := newSiblingCheckFixture(t)
		f.d["body"] = strings.Repeat("保護対象本文", 16000)
		f.persist(t)
		err := f.s.Check(t.Context(), f.d)
		if err == nil || !strings.Contains(err.Error(), "入力上限") || len(f.sent) != 0 {
			t.Fatalf("err=%v calls=%d", err, len(f.sent))
		}
		if obj(f.saved(t)["filingCheckCalls"])["outcome"] == "allowed" {
			t.Fatal("oversized input allowed")
		}
	})
}
