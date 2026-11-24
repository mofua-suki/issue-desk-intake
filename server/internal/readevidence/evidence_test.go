package readevidence

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestReadEvidenceValidation(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{"version": 1, "documents": []any{map[string]any{"id": "a", "project": "owner/repo", "path": "a.go", "revision": strings.Repeat("a", 40), "start": 1, "end": 2, "text": "a\r\nb"}}, "facts": []any{map[string]any{"statement": "fact", "documents": []any{"a"}}}}
	}
	if _, err := Validate(valid(), "owner/repo"); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){"version": func(v map[string]any) { v["version"] = 2 }, "missing": func(v map[string]any) { delete(v, "facts") }, "revision": func(v map[string]any) { v["documents"].([]any)[0].(map[string]any)["revision"] = "main" }, "range": func(v map[string]any) { v["documents"].([]any)[0].(map[string]any)["start"] = 0 }, "reference": func(v map[string]any) { v["facts"].([]any)[0].(map[string]any)["documents"] = []any{"other"} }, "size": func(v map[string]any) {
		v["documents"].([]any)[0].(map[string]any)["text"] = strings.Repeat("a", 65536)
	}}

	cases["duplicate"] = func(v map[string]any) { v["documents"] = append(v["documents"].([]any), v["documents"].([]any)[0]) }
	cases["project"] = func(v map[string]any) { v["documents"].([]any)[0].(map[string]any)["project"] = "other" }
	cases["empty reference"] = func(v map[string]any) { v["facts"].([]any)[0].(map[string]any)["documents"] = []any{} }
	cases["too many documents"] = func(v map[string]any) {
		docs := make([]any, 17)
		for i := range docs {
			docs[i] = map[string]any{"id": fmt.Sprint(i), "project": "owner/repo", "path": "a", "revision": strings.Repeat("a", 40), "start": 1, "end": 1, "text": "x"}
		}
		v["documents"] = docs
	}
	cases["too many facts"] = func(v map[string]any) {
		facts := make([]any, 65)
		for i := range facts {
			facts[i] = map[string]any{"statement": "x", "documents": []any{"a"}}
		}
		v["facts"] = facts
	}
	for _, n := range []int{65536, 65537} {
		v := valid()
		d := v["documents"].([]any)[0].(map[string]any)
		d["text"] = ""
		raw, _ := json.Marshal(v)
		d["text"] = strings.Repeat("x", n-len(raw))
		_, err := Validate(v, "owner/repo")
		if (err == nil) != (n == 65536) {
			t.Fatalf("size %d: %v", n, err)
		}
	}
	max := valid()
	docs := make([]any, 16)
	for i := range docs {
		docs[i] = map[string]any{"id": fmt.Sprint(i), "project": "owner/repo", "path": "a", "revision": strings.Repeat("a", 40), "start": 1, "end": 1, "text": "x"}
	}
	facts := make([]any, 64)
	for i := range facts {
		facts[i] = map[string]any{"statement": "x", "documents": []any{"0"}}
	}
	max["documents"] = docs
	max["facts"] = facts
	if _, err := Validate(max, "owner/repo"); err != nil {
		t.Fatal(err)
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			v := valid()
			change(v)
			if _, err := Validate(v, "owner/repo"); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
}
