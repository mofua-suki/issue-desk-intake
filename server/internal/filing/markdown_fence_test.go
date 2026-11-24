package filing

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestFencedReferenceAndAcceptance(t *testing.T) {
	source := "本文\n~~~\n## 参照（経緯・根拠）\n偽\n~~~\n## 参照（経緯・根拠）\n根拠\n~~~\n## 終了\n残す\n~~~\n## 次\n末尾"
	body, reference := referencePart(source)
	if !strings.Contains(body, "偽") || !strings.Contains(body, "## 次\n末尾") || !strings.Contains(reference, "## 終了\n残す") {
		t.Fatalf("body=%q reference=%q", body, reference)
	}
	text := "~~~\n## 受入条件\n- 偽\n~~~\n## 受入条件\n- 実条件\n~~~\n## 別節\n- 偽条件\n~~~\n- 実条件2\n## 次\n- 対象外"
	if count, found := acceptanceCount(text); !found || count != 2 {
		t.Fatalf("count=%d found=%v", count, found)
	}
}

func TestSharedMarkdownFenceCases(t *testing.T) {
	data, err := os.ReadFile("../../../test/fixtures/markdown-fences.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string   `json:"name"`
		Text   string   `json:"text"`
		Fenced []bool   `json:"fenced"`
		Lines  []string `json:"lines"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			for _, text := range []string{tc.Text, strings.ReplaceAll(tc.Text, "\r\n", "\n")} {
				var got []bool
				var lines []string
				rows := markdownLines(text)
				for i, row := range rows {
					got = append(got, row.fenced)
					line := row.text
					if i < len(rows)-1 {
						line = strings.TrimSuffix(line, "\r")
					}
					lines = append(lines, line)
				}
				if !reflect.DeepEqual(got, tc.Fenced) {
					t.Fatalf("got %v, want %v", got, tc.Fenced)
				}
				if !reflect.DeepEqual(lines, tc.Lines) {
					t.Fatalf("lines %q, want %q", lines, tc.Lines)
				}
			}
		})
	}
}

func TestUnclosedFenceHidesSections(t *testing.T) {
	text := "本文\n```\n## 受入条件\n- 偽\n## 参照（経緯・根拠）\n偽"
	if count, found := acceptanceCount(text); found || count != 0 {
		t.Fatalf("count=%d found=%v", count, found)
	}
	body, reference := referencePart(text)
	if body != text || reference != "" {
		t.Fatalf("body=%q reference=%q", body, reference)
	}
	text = "## 受入条件\n- 実条件\n~~~\n- 偽\n## 参照（経緯・根拠）\n偽"
	if count, found := acceptanceCount(text); !found || count != 1 {
		t.Fatalf("count=%d found=%v", count, found)
	}
}
