package filing

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"strings"
	"testing"
)

func TestProhibitionWithoutExceptionGetsExceptionSentence(t *testing.T) {
	cases := []string{
		"7. 変更は宣言した2ファイル内で行う。go test ./... が通る。",
		"- internal/execution/ai.goは変更しない。",
		"- 範囲外のファイルは変更しない。",
	}
	for _, line := range cases {
		got, changed := allowBrokenReferenceFixes("## 受入条件\n" + line + "\n- 別の項目")
		if !changed || !strings.Contains(got, line+prohibitionException+"\n- 別の項目") {
			t.Fatalf("除外が足されていません: %q → %q", line, got)
		}
	}
}

func TestProhibitionWithExceptionOrWithoutProhibitionIsUnchanged(t *testing.T) {
	for _, body := range []string{
		"- 変更は宣言した範囲内で行う。ただし、切れた参照を直すことは除く。",
		"- 宣言外のファイルは変更しない（その変更で切れたテストの期待値を直すことは除く）。",
		"- go test ./... が通る。",
		"```\n- foo.goは変更しない。\n```",
		"本文の段落で foo.go は変更しない。",
	} {
		if got, changed := allowBrokenReferenceFixes(body); changed || got != body {
			t.Fatalf("変わってはいけません: %q → %q", body, got)
		}
	}
}

func TestScopeHandlingSectionOnlyForKnownScopeAndOnlyOnce(t *testing.T) {
	known := Record{"known": true, "paths": []any{"a.go"}}
	got := withScopeHandling("本文", known)
	if !strings.Contains(got, "テストの削除・省略・弱体化はしない") || withScopeHandling(got, known) != got {
		t.Fatalf("固定の節がありません、または重複します: %q", got)
	}
	for _, scope := range []Record{nil, {"known": false, "paths": []any{}}, {"known": true, "paths": []any{}}} {
		if withScopeHandling("本文", scope) != "本文" {
			t.Fatalf("範囲が未確定のときは足しません: %v", scope)
		}
	}
}

func TestPrepareAddsExceptionAndFixedSectionAndKeepsScopeParsable(t *testing.T) {
	d := draftFixture()
	d["body"] = strings.Replace(fleet.Text(d["body"]), "## 目的", "## 受入条件\n- 変更は宣言したファイル内で行う。\n\n## 目的", 1)
	prepared, e := Prepare(context.Background(), configFixture(), d)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(prepared.Body, "内で行う。"+prohibitionException) || strings.Count(prepared.Body, "## 変更範囲の扱い（受付の固定）") != 1 {
		t.Fatal(prepared.Body)
	}
	if scope := ParseScope(prepared.Body); scope.State != "declared" || len(scope.Paths) != 1 {
		t.Fatal("範囲の宣言を読めません", scope)
	}
	if fleet.Text(d["body"]) == prepared.Body || strings.Contains(fleet.Text(d["body"]), prohibitionException) {
		t.Fatal("保存済みの下書きを書き換えました")
	}
}
