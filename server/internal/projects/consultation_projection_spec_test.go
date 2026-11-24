package projects

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestConsultationProjectionSpec(t *testing.T) {
	// The published distinction must match what the project chat endpoint does.
	spec, err := os.ReadFile("../../../docs/consultation-spec.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{
		"実際にAIへ送る会話投影（発言と参照、および省略位置）のJSONをUTF-16コード単位で計数する",
		"プロジェクト相談の送信受付上限は、保存する発言一覧のJSONをUTF-16コード単位で計数する",
	} {
		if !strings.Contains(string(spec), rule) {
			t.Fatalf("公開仕様に計数対象がありません: %s", rule)
		}
	}
	f := newFixture(t)
	original := []any{Object{"role": "user", "text": "短い発言", "references": []any{Object{"body": strings.Repeat("参", 80000)}}}}
	p := Object{"id": id, "status": "ready", "messages": original}
	if err := f.s.save(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	code, result := f.call("/api/projects/"+id+"/chat", Object{"text": "補足"})
	if code != 400 || result["error"] != "会話が上限を超えています。" {
		t.Fatalf("保存JSONが上限を超える送信: status=%d result=%v", code, result)
	}
	after, err := f.s.load(context.Background(), id)
	if err != nil || !reflect.DeepEqual(after["messages"], original) {
		t.Fatalf("拒否後の保存発言: %v, %v", after, err)
	}
}
