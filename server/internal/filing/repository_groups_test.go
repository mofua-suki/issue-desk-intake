package filing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"strings"
	"testing"
)

func groupFilingFixture() (BuildConfig, Record) {
	c := configFixture()
	c.Config["assignments"] = Record{"repositoryGroups": []any{[]any{"owner/repo", "owner/other"}}}
	d := draftFixture()
	proposal := func(project string) Record {
		return Record{"project": project, "title": "題名", "purpose": "実装", "acceptance": []any{"動く"}, "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"src/a"}}}
	}
	d["splitIssues"] = []any{proposal("owner/repo"), proposal("owner/other")}
	doc := func() Record {
		return Record{"revision": strings.Repeat("a", 40), "files": []any{Record{"path": "README.md", "bytes": 1, "sha256": strings.Repeat("b", 64)}}}
	}
	ev := doc()
	ev["projects"] = Record{"owner/other": doc()}
	d["context"] = ev
	return c, d
}
func approveGroups(d Record, scopes []Record) {
	type scope struct {
		Project string `json:"project"`
		Scope   string `json:"scope"`
	}
	legacy := false
	for _, item := range scopes {
		if strings.HasPrefix(fleet.Text(item["scope"]), "除外理由: ") {
			legacy = true
		}
	}
	if !legacy && len(scopes) > 0 {
		first := 0
		for i := 1; i < len(scopes); i++ {
			if fleet.Text(scopes[i]["project"]) < fleet.Text(scopes[first]["project"]) {
				first = i
			}
		}
		scopes[first]["scope"] = fleet.Text(scopes[first]["scope"]) + "（理由: 連携）"
	}
	var canonical []scope
	var saved []any
	for _, s := range scopes {
		canonical = append(canonical, scope{fleet.Text(s["project"]), fleet.Text(s["scope"])})
		saved = append(saved, s)
	}
	sum := sha256.Sum256([]byte(encoded(canonical)))
	fp := hex.EncodeToString(sum[:])
	d["crossProposal"] = Record{"fingerprint": fp, "reason": "連携", "scopes": saved}
	d["crossApprovals"] = []any{Record{"fingerprint": fp, "scopes": saved, "exceptionExplicit": true, "exceptionApproval": Record{"via": "screen", "fingerprint": fp}}}
}
func TestRepositoryGroupsFilingGate(t *testing.T) {
	c, d := groupFilingFixture()
	ctx := context.Background()
	d["splitIssues"] = nil
	if ValidateSplit(ctx, c, d) == nil {
		t.Fatal("片側の通常起票を受理")
	}
	c, d = groupFilingFixture()
	d["splitIssues"] = fleet.Elements(d["splitIssues"])[:1]
	if ValidateSplit(ctx, c, d) == nil {
		t.Fatal("片側の分割起票を受理")
	}
	c, d = groupFilingFixture()
	approveGroups(d, []Record{{"project": "owner/other", "scope": "接続"}, {"project": "owner/repo", "scope": "実装"}})
	delete(row(row(d["context"])["projects"]), "owner/other")
	if ValidateSplit(ctx, c, d) == nil {
		t.Fatal("資料不足を受理")
	}
	c, d = groupFilingFixture()
	approveGroups(d, []Record{{"project": "owner/other", "scope": "接続"}, {"project": "owner/repo", "scope": "実装"}})
	file := row(fleet.Elements(row(row(row(d["context"])["projects"])["owner/other"])["files"])[0])
	file["bytes"] = 0
	if ValidateSplit(ctx, c, d) == nil {
		t.Fatal("空の読取を資料本文として受理")
	}
	c, d = groupFilingFixture()
	c.Project = func(name string) error {
		if name == "owner/other" {
			return errors.New("許可対象外")
		}
		return nil
	}
	if ValidateSplit(ctx, c, d) == nil {
		t.Fatal("候補外を受理")
	}
}
func TestRepositoryGroupsFilingApproval(t *testing.T) {
	c, d := groupFilingFixture()
	ctx := context.Background()
	if ValidateSplit(ctx, c, d) == nil {
		t.Fatal("了承なしを受理")
	}
	approveGroups(d, []Record{{"project": "owner/other", "scope": "接続"}, {"project": "owner/repo", "scope": "実装"}})
	if err := ValidateSplit(ctx, c, d); err != nil {
		t.Fatal(err)
	}
	row(d["crossProposal"])["reason"] = "変更された理由"
	if ValidateSplit(ctx, c, d) == nil {
		t.Fatal("変更された理由を受理")
	}
	row(d["crossProposal"])["reason"] = "連携"
	row(fleet.Elements(row(fleet.Elements(d["crossApprovals"])[0])["scopes"])[0])["scope"] = "別の範囲"
	if ValidateSplit(ctx, c, d) == nil {
		t.Fatal("異なる範囲の了承を受理")
	}
}
func TestRepositoryGroupsFilingException(t *testing.T) {
	ctx := context.Background()
	for _, split := range []bool{false, true} {
		c, d := groupFilingFixture()
		if split {
			d["splitIssues"] = fleet.Elements(d["splitIssues"])[:1]
		} else {
			d["splitIssues"] = nil
		}
		scopes := []Record{{"project": "owner/other", "scope": "除外理由: 変更不要 / 条件: 契約が変わらない"}, {"project": "owner/repo", "scope": "実装"}}
		if ValidateSplit(ctx, c, d) == nil {
			t.Fatal("了承なし")
		}
		approveGroups(d, scopes)
		receipt := row(fleet.Elements(d["crossApprovals"])[0])["exceptionApproval"]
		delete(row(fleet.Elements(d["crossApprovals"])[0]), "exceptionApproval")
		if ValidateSplit(ctx, c, d) == nil {
			t.Fatal("出典がない了承を受理")
		}
		row(fleet.Elements(d["crossApprovals"])[0])["exceptionApproval"] = receipt
		if err := ValidateSplit(ctx, c, d); err != nil {
			t.Fatalf("split=%v: %v", split, err)
		}
		row(fleet.Elements(row(fleet.Elements(d["crossApprovals"])[0])["scopes"])[0])["scope"] = "除外理由: 修正 / 条件: 契約が変わらない"
		if ValidateSplit(ctx, c, d) == nil {
			t.Fatal("変更後の除外条件を受理")
		}
		approveGroups(d, []Record{{"project": "owner/other", "scope": "除外理由: 変更不要 / 条件: "}, {"project": "owner/repo", "scope": "実装"}})
		if ValidateSplit(ctx, c, d) == nil {
			t.Fatal("条件なし")
		}
	}
}

func TestRepositoryGroupsFilingConfigurationChange(t *testing.T) {
	c, d := groupFilingFixture()
	ctx := context.Background()
	approveGroups(d, []Record{{"project": "owner/other", "scope": "接続"}, {"project": "owner/repo", "scope": "実装"}})
	if err := ValidateSplit(ctx, c, d); err != nil {
		t.Fatal(err)
	}
	c.Config["assignments"] = Record{"repositoryGroups": []any{[]any{"owner/repo", "owner/other", "owner/third"}}}
	c.Project = func(string) error { return nil }
	if ValidateSplit(ctx, c, d) == nil {
		t.Fatal("変更後の組を検査しなかった")
	}
	c.Config["assignments"] = Record{"repositoryGroups": []any{[]any{"owner/repo", "owner/other"}}}
	c.Project = func(name string) error {
		if name == "owner/other" {
			return errors.New("候補外")
		}
		return nil
	}
	if ValidateSplit(ctx, c, d) == nil {
		t.Fatal("変更後の許可候補を検査しなかった")
	}
}

func TestRepositoryGroupsFilingSavedChildPostgres(t *testing.T) {
	s, api := fixture(t)
	ctx := context.Background()
	c, parent := groupFilingFixture()
	approveGroups(parent, []Record{{"project": "owner/other", "scope": "接続"}, {"project": "owner/repo", "scope": "実装"}})
	childID := "22222222-2222-4222-8222-222222222222"
	parent["splitChildren"] = []any{childID, "33333333-3333-4333-8333-333333333333"}
	_, err := s.options.Records.Change(ctx, "drafts", draftID, func(_ *sql.Tx, _ Record) (Record, error) { return parent, nil })
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.options.Records.Change(ctx, "drafts", childID, func(_ *sql.Tx, _ Record) (Record, error) {
		return Record{"id": childID, "project": "owner/repo", "parentSplit": draftID, "splitIndex": 1, "status": "draft", "revision": 0}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	child, found, err := s.options.Records.Get(ctx, "drafts", childID)
	if err != nil || !found {
		t.Fatal(err)
	}
	c.Records = s.options.Records
	if err = ValidateSplit(ctx, c, child); err != nil {
		t.Fatal(err)
	}
	c.Project = func(repo string) error {
		if repo == "owner/other" {
			return errors.New("候補外")
		}
		return nil
	}
	s.options.Check = func(ctx context.Context, d Record) error { return ValidateSplit(ctx, c, d) }
	if _, err = s.normal(ctx, child, false); err == nil {
		t.Fatal("現在の許可候補を検査しなかった")
	}
	if api.posts != 0 {
		t.Fatal("候補外へ起票した")
	}
	c.Project = func(string) error { return nil }
	child, found, err = s.options.Records.Get(ctx, "drafts", childID)
	if err != nil || !found {
		t.Fatal(err)
	}
	if _, err = s.normal(ctx, child, false); err != nil {
		t.Fatal(err)
	}
	if api.posts != 1 {
		t.Fatal("了承済みの子を起票しなかった")
	}
}

func TestRepositoryGroupsFilingShippedConfiguration(t *testing.T) {
	raw, err := os.ReadFile("../../../assignments.json")
	if err != nil {
		t.Fatal(err)
	}
	var assignments map[string]any
	if err = json.Unmarshal(raw, &assignments); err != nil {
		t.Fatal(err)
	}
	c, d := groupFilingFixture()
	c.Config["assignments"] = assignments
	c.Project = func(string) error { return nil }
	d["project"] = "mofua-suki/sample-expense-server"
	d["splitIssues"] = nil
	if ValidateSplit(context.Background(), c, d) == nil {
		t.Fatal("出荷設定の片側起票を受理")
	}
}

func TestRepositoryGroupsFilingMixedTargets(t *testing.T) {
	c, d := groupFilingFixture()
	c.Project = func(string) error { return nil }
	extra := clone(row(fleet.Elements(d["splitIssues"])[0]))
	extra["project"] = "owner/third"
	d["splitIssues"] = append(fleet.Elements(d["splitIssues"]), extra)
	row(row(d["context"])["projects"])["owner/third"] = Record{"revision": strings.Repeat("c", 40), "files": []any{Record{"path": "README.md", "bytes": 1, "sha256": strings.Repeat("d", 64)}}}
	approveGroups(d, []Record{{"project": "owner/other", "scope": "接続"}, {"project": "owner/repo", "scope": "実装"}})
	if ValidateSplit(context.Background(), c, d) == nil {
		t.Fatal("組外対象の了承なし")
	}
	approveGroups(d, []Record{{"project": "owner/other", "scope": "接続"}, {"project": "owner/repo", "scope": "実装"}, {"project": "owner/third", "scope": "連携"}})
	if err := ValidateSplit(context.Background(), c, d); err != nil {
		t.Fatal(err)
	}
}

func nestedGroupRecords(t *testing.T) (*Service, BuildConfig, Record, Record, Record) {
	t.Helper()
	s, _ := fixture(t)
	c, root := groupFilingFixture()
	c.Records = s.options.Records
	root["id"] = draftID
	middleID := "22222222-2222-4222-8222-222222222222"
	leafID := "33333333-3333-4333-8333-333333333333"
	root["splitChildren"] = []any{middleID, "44444444-4444-4444-8444-444444444444"}
	middle := Record{"id": middleID, "project": "owner/repo", "parentSplit": draftID, "splitIndex": 1, "splitChildren": []any{leafID}, "splitIssues": []any{clone(row(fleet.Elements(root["splitIssues"])[0]))}}
	leaf := Record{"id": leafID, "project": "owner/repo", "parentSplit": middleID, "splitIndex": 1}
	return s, c, root, middle, leaf
}

func saveNestedRecord(t *testing.T, s *Service, d Record) {
	t.Helper()
	_, err := s.options.Records.Change(context.Background(), "drafts", fleet.Text(d["id"]), func(_ *sql.Tx, _ Record) (Record, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestNestedSplitRejectsInvalidIntermediatePlan(t *testing.T) {
	s, c, root, middle, leaf := nestedGroupRecords(t)
	c.Project = func(string) error { return nil }
	approveGroups(root, []Record{{"project": "owner/other", "scope": "接続"}, {"project": "owner/repo", "scope": "実装"}})
	saveNestedRecord(t, s, root)
	saveNestedRecord(t, s, middle)
	ctx := context.Background()
	if err := ValidateSplit(ctx, c, leaf); err != nil {
		t.Fatalf("有効な祖先の計画: %v", err)
	}
	row(fleet.Elements(middle["splitIssues"])[0])["title"] = ""
	saveNestedRecord(t, s, middle)
	leaf["status"], leaf["revision"] = "draft", 0
	saveNestedRecord(t, s, leaf)
	s.options.Check = func(ctx context.Context, d Record) error { return ValidateSplit(ctx, c, d) }
	if _, err := s.normal(ctx, leaf, false); err == nil {
		t.Fatal("不正な途中の計画を持つ子を受理")
	}
	if s.options.API.(*memoryAPI).posts != 0 {
		t.Fatal("不正な計画から子を起票した")
	}
}

func TestRepositoryGroupsAllowNestedSplitChild(t *testing.T) {
	s, c, root, middle, leaf := nestedGroupRecords(t)
	c.Project = func(string) error { return nil }
	ctx := context.Background()
	// Exercise the repository groups shipped with the application as well.
	configured := c.Config["assignments"]
	raw, err := os.ReadFile("../../../assignments.json")
	if err != nil {
		t.Fatal(err)
	}
	var shipped map[string]any
	if err := json.Unmarshal(raw, &shipped); err != nil {
		t.Fatal(err)
	}
	c.Config["assignments"] = shipped
	root["project"] = "owner/outside"
	row(fleet.Elements(root["splitIssues"])[0])["project"] = "owner/outside"
	root["splitIssues"] = fleet.Elements(root["splitIssues"])[:1]
	middle["project"], leaf["project"] = "owner/outside", "owner/outside"
	row(fleet.Elements(middle["splitIssues"])[0])["project"] = "owner/outside"
	saveNestedRecord(t, s, root)
	saveNestedRecord(t, s, middle)
	if err := ValidateSplit(ctx, c, leaf); err != nil {
		t.Fatal(err)
	}
	leaf["status"], leaf["revision"] = "draft", 0
	saveNestedRecord(t, s, leaf)
	s.options.Check = func(ctx context.Context, d Record) error { return ValidateSplit(ctx, c, d) }
	if _, err := s.normal(ctx, leaf, false); err != nil {
		t.Fatal(err)
	}
	if s.options.API.(*memoryAPI).posts != 1 {
		t.Fatal("子の子を起票しなかった")
	}
	c.Config["assignments"] = configured
	_, grouped := groupFilingFixture()
	grouped["id"], grouped["splitChildren"] = root["id"], root["splitChildren"]
	middle["project"], leaf["project"] = "owner/repo", "owner/repo"
	row(fleet.Elements(middle["splitIssues"])[0])["project"] = "owner/repo"
	saveNestedRecord(t, s, middle)
	grouped["splitIssues"] = fleet.Elements(grouped["splitIssues"])[:1]
	saveNestedRecord(t, s, grouped)
	if ValidateSplit(ctx, c, leaf) == nil {
		t.Fatal("組の対象不足を受理")
	}
	_, grouped = groupFilingFixture()
	grouped["id"], grouped["splitChildren"] = root["id"], root["splitChildren"]
	saveNestedRecord(t, s, grouped)
	if ValidateSplit(ctx, c, leaf) == nil {
		t.Fatal("了承不足を受理")
	}
	approveGroups(grouped, []Record{{"project": "owner/other", "scope": "接続"}, {"project": "owner/repo", "scope": "実装"}})
	other := row(row(grouped["context"])["projects"])["owner/other"]
	delete(row(row(grouped["context"])["projects"]), "owner/other")
	saveNestedRecord(t, s, grouped)
	if ValidateSplit(ctx, c, leaf) == nil {
		t.Fatal("資料不足を受理")
	}
	row(row(grouped["context"])["projects"])["owner/other"] = other
	saveNestedRecord(t, s, grouped)
	if err := ValidateSplit(ctx, c, leaf); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryGroupsNestedSplitInvalidAncestry(t *testing.T) {
	s, c, root, middle, leaf := nestedGroupRecords(t)
	approveGroups(root, []Record{{"project": "owner/other", "scope": "接続"}, {"project": "owner/repo", "scope": "実装"}})
	saveNestedRecord(t, s, root)
	saveNestedRecord(t, s, middle)
	for _, tc := range []struct {
		name   string
		change func(Record)
	}{
		{"missing", func(d Record) { d["parentSplit"] = "99999999-9999-4999-8999-999999999999" }},
		{"id", func(d Record) { d["id"] = "99999999-9999-4999-8999-999999999999" }},
		{"index", func(d Record) { d["splitIndex"] = 2 }},
		{"target", func(d Record) { d["project"] = "owner/other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := clone(leaf)
			tc.change(d)
			if ValidateSplit(context.Background(), c, d) == nil {
				t.Fatal("不正な親子関係を受理")
			}
		})
	}
	middle["parentSplit"] = fleet.Text(leaf["id"])
	leaf["splitChildren"] = []any{middle["id"]}
	leaf["splitIssues"] = middle["splitIssues"]
	saveNestedRecord(t, s, middle)
	saveNestedRecord(t, s, leaf)
	if ValidateSplit(context.Background(), c, leaf) == nil {
		t.Fatal("循環を受理")
	}
}
