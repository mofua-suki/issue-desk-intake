package consultationai

import (
	"encoding/json"
	"fmt"
	"issue-desk-intake/server/internal/fleet"
	"reflect"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

func TestFileCandidatesInFullAndResumedInputs(t *testing.T) {
	p := &projectState{Name: "owner/app", entries: map[string]treeEntry{}}
	for i := 0; i < 2500; i++ {
		name := fmt.Sprintf("src/package-%04d/long-file-name.go", i)
		p.entries[name] = treeEntry{Path: name}
	}
	last := "src/package-2499/long-file-name.go"
	d := Record{"project": "owner/app", "messages": []any{Record{"text": "対象の" + last + "を変更"}}}
	m := &materials{draft: d, state: contextState{Projects: map[string]*projectState{"owner/app": p}}}
	oldBytes := len(marshal(sortedFiles(p)))
	if oldBytes < 40*1024 {
		t.Fatal(oldBytes)
	}
	data, shown, _ := m.data(false)
	project := fleet.Object(fleet.Elements(data["projects"])[0])
	files := project["availableFiles"].([]string)
	if len(files) == 0 || files[0] != last || project["availableFilesOmitted"] != true || project["availableFileCount"] != len(p.entries) {
		t.Fatalf("候補: %+v", project)
	}
	for _, state := range []*sessionState{nil, {}} {
		full := prompt(d, data, nil, 1, nil, "")
		input := resumedPrompt(full, d, m, state, shown, turnPromptState{})
		var usage ai.Usage
		measureInput(ai.Request{Input: input}, &usage, nil)
		if usage.PromptBytes["fileList"] == nil {
			t.Fatal("一覧の計測なし")
		}
		got := *usage.PromptBytes["fileList"]
		if got != int64(len(marshal(files))) || got > 8192 || got*5 > int64(oldBytes) {
			t.Fatalf("一覧 bytes=%d old=%d", got, oldBytes)
		}
	}
	data2, _, _ := m.data(false)
	if string(marshal(data)) != string(marshal(data2)) {
		t.Fatal("候補順が不安定")
	}
	full := prompt(d, data, nil, 1, nil, "")
	resume := resumedPrompt(full, d, m, &sessionState{ID: "session"}, shown, turnPromptState{})
	var usage ai.Usage
	measureInput(ai.Request{Input: resume}, &usage, nil)
	if usage.PromptBytes["fileList"] == nil || *usage.PromptBytes["fileList"] != 0 {
		t.Fatalf("再開差分: %+v", usage.PromptBytes)
	}

	p.entries = map[string]treeEntry{"b.go": {}, "a.go": {}}
	small, _, _ := m.data(false)
	smallProject := fleet.Object(fleet.Elements(small["projects"])[0])
	if !reflect.DeepEqual(smallProject["availableFiles"], []string{"a.go", "b.go"}) || smallProject["availableFilesOmitted"] != false {
		t.Fatalf("小規模: %+v", smallProject)
	}
}

func TestPromptIncludesSavedPlans(t *testing.T) {
	d := Record{"splitIssues": []any{Record{"project": "owner/app", "title": "一", "purpose": "内容", "acceptance": []any{"確認"}, "changeScope": Record{"known": true, "paths": []any{"a.go"}, "reason": ""}, "dependsOnIssues": []any{3}, "dependsOn": []any{}}, Record{"project": "owner/app", "title": "二", "purpose": "次", "acceptance": []any{"検証"}, "changeScope": Record{"known": true, "paths": []any{"b.go"}, "reason": ""}, "dependsOnIssues": []any{}, "dependsOn": []any{1}}}, "changeScope": Record{"known": true}, "dependsOnIssues": []any{3}, "messages": []any{}}
	full := prompt(d, Record{}, nil, 0, nil, "")
	resumed := resumedPrompt(full, d, &materials{}, &sessionState{ID: "session"}, nil, turnPromptState{})
	for _, tc := range []struct{ name, input, key string }{{"current", full, "current"}, {"latest", resumed, "latest"}} {
		t.Run(tc.name, func(t *testing.T) {
			start := strings.Index(tc.input, "\n{")
			if start < 0 {
				t.Fatal("JSON missing")
			}
			var data map[string]any
			if err := json.Unmarshal([]byte(tc.input[start+1:]), &data); err != nil {
				t.Fatal(err)
			}
			got := data[tc.key].(map[string]any)
			for _, key := range []string{"splitIssues", "changeScope", "dependsOnIssues"} {
				var expected any
				raw, _ := json.Marshal(d[key])
				_ = json.Unmarshal(raw, &expected)
				if !reflect.DeepEqual(got[key], expected) {
					t.Fatalf("%s: got %#v want %#v", key, got[key], expected)
				}
			}
		})
	}
}

func TestPromptBudgetChangesPreserveMaterialsPrefix(t *testing.T) {
	d := Record{"project": "repo", "provider": "codex", "messages": []any{Record{"text": "依頼"}}, "title": "題名", "body": "本文", "questions": []any{"質問"}, "tier": "standard", "references": []any{"参照"}, "splitIssues": []any{Record{"title": "保存済み", "purpose": "内容", "dependsOn": []any{}, "changeScope": Record{"known": true, "paths": []any{"a.go"}}, "dependsOnIssues": []any{123}}}, "changeScope": Record{"known": true, "paths": []any{"a.go"}}, "dependsOnIssues": []any{123}}
	context := Record{"projects": []any{Record{"documents": []any{Record{"path": "a", "text": strings.Repeat("資料", 1000)}}, "availableFiles": []any{"a", "b"}}}}
	budgets := []any{nil, Record{"used": Record{"calls": 8}, "remaining": Record{"calls": 2}}, Record{"used": Record{"calls": 9}, "remaining": Record{"calls": 1}}}
	const marker = `,"consultationBudget":`
	var prefix string
	for _, budget := range budgets {
		got := prompt(d, context, budget, 2, nil, "注記")
		before, _, ok := strings.Cut(got, marker)
		if !ok {
			t.Fatal("budget missing")
		}
		if prefix != "" && before != prefix {
			t.Fatal("budget changed the materials prefix")
		}
		prefix = before
		var actual, expected map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(got, turnInstructions+"\n以下は相談と資料のデータです:\n")), &actual); err != nil {
			t.Fatal(err)
		}
		want := Record{"project": d["project"], "provider": d["provider"], "messages": d["messages"], "current": Record{"title": d["title"], "body": d["body"], "questions": d["questions"], "splitIssues": d["splitIssues"], "changeScope": d["changeScope"], "dependsOnIssues": d["dependsOnIssues"]}, "tier": d["tier"], "context": context, "consultationBudget": budget, "images": []Record(nil), "remainingReadRounds": 2, "references": d["references"], "notes": "注記", "controllerVerification": Record{"fixedGateOwner": "controller", "planLocation": "completionPlan.verification", "successRequiredBeforeMerge": true, "duplicateImplementerRequirement": false, "preserveExplicitAcceptanceChecks": true}}
		if err := json.Unmarshal(marshal(want), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("prompt data changed: got %v want %v", actual, expected)
		}
	}
	changed := prompt(d, context, budgets[0], 1, nil, "注記")
	if strings.HasPrefix(changed, prefix+marker) {
		t.Fatal("remaining read rounds must stay current")
	}
}

func TestResumedPromptResendsFullListWhenItReturnsToAnEarlierVersion(t *testing.T) {
	const repo = "owner/app"
	d := Record{"project": repo}
	a := Document{Project: repo, Path: "open-requests", Request: ReadRequest{File: "open-requests", Mode: "full"}, Text: "再び現在の依頼一覧"}
	a.SHA256 = digest(a.Text)
	b := a
	b.Text, b.SHA256 = "直前の依頼一覧", digest("直前の依頼一覧")
	p := &projectState{Name: repo, Reads: []Document{a}}
	m := &materials{draft: d, state: contextState{Projects: map[string]*projectState{repo: p}}}
	_, shown, _ := m.data(false)
	state := &sessionState{
		ID: "session", LastOpenRequestsKey: documentKey(b),
		Sent: map[string]bool{documentKey(a): true, documentKey(b): true},
	}
	input := resumedPrompt("", d, m, state, shown, turnPromptState{})
	if !strings.Contains(input, a.Text) || strings.Contains(input, b.Text) {
		t.Fatalf("以前の版に戻った現在の一覧を提示していません: %s", input)
	}
	state.LastOpenRequestsKey = documentKey(a)
	if repeated := resumedPrompt("", d, m, state, shown, turnPromptState{}); strings.Contains(repeated, a.Text) {
		t.Fatal("直前と同じ一覧をもう一度提示しました")
	}
}
