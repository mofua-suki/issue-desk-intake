package filingcheck

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-intake/server/internal/filing"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
	"time"
)

type failingEvidenceReader struct {
	delegate evidenceReader
	err      error
	calls    []string
}

func (r *failingEvidenceReader) Get(ctx context.Context, table, id string) (Record, bool, error) {
	r.calls = append(r.calls, table+":"+id)
	if id == "broken" && r.err != nil {
		return nil, false, r.err
	}
	return r.delegate.Get(ctx, table, id)
}

func TestEvidenceReadFailureReleasesPending(t *testing.T) {
	for _, outside := range []bool{false, true} {
		t.Run(fmt.Sprintf("outside=%t", outside), func(t *testing.T) {
			calls := 0
			s, d, material := fixture(t, func(_ context.Context, _ map[string]any) (map[string]any, error) {
				calls++
				out := allowed()
				if calls <= 2 {
					out["proceed"] = false
					id := "request:3"
					if calls == 2 {
						id = "consultation:broken"
					}
					out["evidenceRequests"] = []any{Record{"candidateId": id, "kind": "body"}}
				}
				return out, nil
			})
			if !outside {
				(*material)["consultations"] = []any{Record{"id": "broken", "revision": 1}}
			}
			ctx := context.Background()
			if _, err := s.options.Records.Change(ctx, "drafts", "broken", func(*sql.Tx, Record) (Record, error) {
				return Record{"id": "broken", "revision": 1, "project": d["project"], "body": "## 目的\n本文"}, nil
			}); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected evidence read failure")
			reader := &failingEvidenceReader{delegate: s.evidenceReads, err: injected}
			s.evidenceReads = reader
			if err := s.Check(ctx, d, "manual"); !errors.Is(err, injected) || calls != 2 || len(reader.calls) != 1 || reader.calls[0] != "drafts:broken" {
				t.Fatal(err, calls)
			}
			saved, _, err := s.options.Records.Get(ctx, "drafts", fleet.Text(d["id"]))
			if err != nil {
				t.Fatal(err)
			}
			state := obj(saved["filingCheckCalls"])
			if state["pending"] != false || state["outcome"] != "evidence_requested" || state["stopKind"] == "evidence_missing" {
				t.Fatal(state)
			}
			reader.err = nil
			if err := s.Check(ctx, saved, "manual"); err != nil || calls != 3 {
				t.Fatal(err, calls)
			}
		})
	}
}

func TestEvidenceExpandedOverInputLimitDropsOlderEvidenceAndContinues(t *testing.T) {
	calls := 0
	s, d, material := fixture(t, func(_ context.Context, input map[string]any) (map[string]any, error) {
		calls++
		out := allowed()
		if calls == 3 {
			var sent Record
			if err := json.Unmarshal([]byte(strings.SplitN(fleet.Text(input["input"]), "\n確認資料:\n", 2)[1]), &sent); err != nil {
				t.Fatal(err)
			}
			evidence := fleet.Elements(sent["evidence"])
			if len(evidence) != 1 || obj(evidence[0])["candidateId"] != "request:4" {
				t.Fatal("直前の要求の資料を残していない", sent["evidence"])
			}
			omitted := string(raw(sent[evidenceOmittedKey]))
			if !strings.Contains(omitted, "request:3") || !strings.Contains(omitted, omissionInputLimitReason) {
				t.Fatal(omitted)
			}
			return out, nil
		}
		out["proceed"] = false
		out["evidenceRequests"] = []any{Record{"candidateId": fmt.Sprintf("request:%d", calls+2), "kind": "body"}}
		return out, nil
	})
	// The original target fits, but two individually valid evidence bodies together exceed the total input budget.
	obj((*material)["target"])["body"] = strings.Repeat("t", inputLimit-len(instructions)-len(s.schema)-47000)
	(*material)["requests"] = []any{Record{"number": 3, "body": strings.Repeat("a", 25000)}, Record{"number": 4, "body": strings.Repeat("b", 25000)}}
	if err := s.Check(context.Background(), d); err != nil || calls != 3 {
		t.Fatal(err, calls)
	}
	saved, _, err := s.options.Records.Get(context.Background(), "drafts", fleet.Text(d["id"]))
	if err != nil {
		t.Fatal(err)
	}
	checks := fleet.Elements(obj(saved["filingCheckCalls"])["checks"])
	if last := obj(checks[len(checks)-1]); !strings.Contains(string(raw(last["inputOmissions"])), omissionInputLimitReason) {
		t.Fatal(last)
	}
}

func TestReferenceBoundariesAndSelectionDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		removed    bool
	}{
		{"details", "## 目的\n本文\n<details>\n<summary>受付の経緯・根拠（参照）</summary>\n## 目的\n参照\n</details>\n## 依存\n後続", true},
		{"escaped details", "## 目的\n本文\n<details>\n<summary>受付の経緯・根拠（参照）</summary>\n## 目的\n参照\n&lt;/details>\n## 依存\n後続", true},
		{"legacy", "## 目的\n本文\n## 受付の経緯・根拠\n参照\n## 依存\n後続", true},
		{"ordinary", "## 目的\n本文\n<details>\n<summary>普通</summary>\n参照\n</details>", false},
		{"fence", "## 目的\n本文\n```\n<!-- issue-desk-reference:start -->\n参照\n<!-- issue-desk-reference:end -->\n```", false},
		{"incomplete", "## 目的\n本文\n<!-- issue-desk-reference:start -->\n参照\n## 依存\n後続", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, ref := splitReference(tc.body)
			if (ref != nil) != tc.removed || !strings.Contains(body, "本文") || (strings.Contains(tc.body, "後続") && !strings.Contains(body, "後続")) {
				t.Fatal(body, ref)
			}
			if tc.name == "escaped details" {
				selected, _, err := selectText(body, Record{"section": "目的"}, true)
				if err != nil || !strings.Contains(selected, "本文") || strings.Contains(selected, "参照") || !strings.Contains(fleet.Text(ref), "参照") {
					t.Fatal(selected, body, ref, err)
				}
			}
		})
	}
	for _, tc := range []struct {
		source    string
		selection Record
		reason    string
	}{
		{"## 他\n本文", Record{"section": "目的"}, "節が存在"},
		{"## 目的\na\n## 目的\nb", Record{"section": "目的"}, "節が重複"},
		{"```\na\n```", Record{"startLine": 2, "endLine": 2}, "コード囲み"},
		{"本文", Record{"startLine": 1, "endLine": 5}, "不正な範囲"},
	} {
		_, _, err := selectText(tc.source, tc.selection, false)
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Fatal(err)
		}
	}
}

func TestPrepareReferenceSelection(t *testing.T) {
	d := filing.Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/repo", "provider": "codex", "title": "実装", "body": "## 目的\n本文の目的\n<details>\n<summary>x</summary>\n内容\n</details>\n## 受入条件\n受入", "ready": true, "questions": []any{}, "changeScope": filing.Record{"known": true, "paths": []any{"src/a.go"}}, "context": filing.Record{"branch": "main"}}
	c := filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }, Project: func(string) error { return nil }, ContextEvidence: func(filing.Record) string {
		return "<details>\n<summary>x</summary>\n内容\n</details>\n## 目的\n分割案1\n## 目的\n分割案2\n```\n未終了"
	}}
	p, err := filing.Prepare(context.Background(), c, d)
	if err != nil {
		t.Fatal(err)
	}
	body, ref := splitReference(p.Body)
	selected, _, err := selectText(body, Record{"section": "目的"}, true)
	if err != nil || !strings.Contains(selected, "本文の目的") || strings.Contains(selected, "分割案") || !strings.Contains(body, "変更範囲") || !strings.Contains(fleet.Text(ref), "分割案2") {
		t.Fatal(selected, body, ref, err)
	}
}

func TestPrepareUnclosedBodyFenceReference(t *testing.T) {
	for _, fence := range []string{"```", "````\n```", "```\n~~~", "~~~~\n~~~"} {
		t.Run(strings.ReplaceAll(fence, "\n", "_"), func(t *testing.T) {
			d := filing.Record{"id": "11111111-1111-4111-8111-111111111111", "project": "owner/repo", "provider": "codex", "title": "実装", "body": "## 目的\n本文の目的\n## 受入条件\n後続節\n" + fence + "\n本文コード\n<!-- issue-desk-reference:start -->\n利用者文字列", "ready": true, "questions": []any{}, "changeScope": filing.Record{"known": true, "paths": []any{"src/a.go"}}, "context": filing.Record{"branch": "main"}}
			c := filing.BuildConfig{Config: map[string]any{"model": "gpt-6.1"}, Now: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }, Project: func(string) error { return nil }, ContextEvidence: func(filing.Record) string { return "## 目的\n参照一\n## 目的\n参照二" }}
			p, err := filing.Prepare(context.Background(), c, d)
			if err != nil {
				t.Fatal(err)
			}
			body, ref := splitReference(p.Body)
			selected, _, err := selectText(body, Record{"section": "目的"}, true)
			if err != nil || !strings.Contains(selected, "本文の目的") || !strings.Contains(body, "後続節") || !strings.Contains(body, "本文コード") || !strings.Contains(body, "&lt;!-- issue-desk-reference:start -->") || !strings.Contains(fleet.Text(ref), "参照一") || !strings.Contains(fleet.Text(ref), "参照二") || strings.Contains(body, "参照二") {
				t.Fatal(body, ref, selected, err)
			}
		})
	}
}
