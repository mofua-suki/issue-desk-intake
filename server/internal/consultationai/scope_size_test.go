package consultationai

import (
	"context"
	"issue-desk-intake/server/internal/fleet"
	"os"
	"regexp"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
)

func TestScopeSizeGuidanceSource(t *testing.T) {
	raw, err := os.ReadFile("../../../shared/src/intake-request-size.ts")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`files:\s*(\d+)`).FindSubmatch(raw)
	if len(match) != 2 || string(match[1]) != intakeRequestFilesText {
		t.Fatalf("source: %s", match)
	}
	for _, phrase := range []string{"4件を目安", "件数だけを理由", "元の対象・受入条件・依存と共通の出典規則"} {
		if !strings.Contains(instructions, phrase) {
			t.Fatalf("missing guidance: %s", phrase)
		}
	}
}
func TestScopeSizeTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []any
		child bool
		want  int
		read  bool
	}{
		{"normal four", []any{"a", "b", "c", "d"}, false, 4, false},
		{"normal five", []any{"a", "b", "c", "d", "e"}, false, 5, false},
		{"normal multiple with duplicate", []any{"a", "A", "b", "c", "d", "e", "f"}, false, 6, false},
		{"split four", []any{"a", "b", "c", "d"}, true, 4, false},
		{"split five", []any{"a", "b", "c", "d", "e"}, true, 5, false},
		{"split multiple with duplicate", []any{"a", "A", "b", "c", "d", "e", "f"}, true, 6, false},
		{"after additional read", []any{"a", "b", "c", "d", "e"}, false, 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
				calls++
				if strings.Contains(r.Input, "変更範囲が目安を超えています") {
					t.Fatal("unexpected size repair")
				}
				v := reply()
				v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
				scope := Record{"known": true, "paths": tc.paths, "reason": ""}
				v["changeScope"] = scope
				if tc.child {
					v["splitIssues"] = []any{Record{"title": "子1", "changeScope": scope}, Record{"title": "子2", "changeScope": scope}}
				}
				if tc.read && calls == 1 {
					v["readFiles"] = []any{Record{"file": "README.md", "mode": "lines", "start": 1, "end": 1}}
				}
				return measuredAnswer(v), nil
			})
			got, err := s.Turn(context.Background(), d)
			wantCalls := 2
			if tc.read {
				wantCalls = 3
			}
			if err != nil || got["ready"] != true || calls != wantCalls {
				t.Fatalf("calls=%d result=%v err=%v", calls, got, err)
			}
			scopes := []any{got["changeScope"]}
			if tc.child {
				scopes = nil
				for _, child := range fleet.Elements(got["splitIssues"]) {
					scopes = append(scopes, fleet.Object(child)["changeScope"])
				}
				if len(scopes) != 2 {
					t.Fatal(got)
				}
			}
			for _, scope := range scopes {
				if paths := fleet.Elements(fleet.Object(scope)["paths"]); len(paths) != tc.want {
					t.Fatalf("paths=%v", paths)
				}
			}
		})
	}
}

func TestScopeSizeValidationBoundaries(t *testing.T) {
	unknown := map[string]any{"known": false, "paths": []any{"a", "b", "c", "d", "e"}}
	if err := validateScope(unknown); err != nil || len(fleet.Elements(unknown["paths"])) != 0 || fleet.Text(unknown["reason"]) == "" {
		t.Fatalf("unknown=%v err=%v", unknown, err)
	}
	directory := map[string]any{"known": true, "paths": []any{"src/"}}
	if err := validateScope(directory); err == nil {
		t.Fatal("directory requires reason")
	}
	directory["reason"] = "範囲全体"
	if err := validateScope(directory); err != nil {
		t.Fatal(err)
	}
}

// A constant can only be initialized from another constant.
const _ = instructions

func TestScopeSizeNormalizedBeforeRepair(t *testing.T) {
	calls := 0
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		calls++
		v := reply()
		v["ready"], v["title"], v["body"], v["questions"] = true, "題名", "目的と受入条件", []any{}
		v["changeScope"] = Record{"known": true, "paths": []any{"src/a.go", "SRC/A.go", "src/b.go", "src/c.go", "src/d.go"}, "reason": ""}
		return measuredAnswer(v), nil
	})
	got, err := s.Turn(context.Background(), d)
	if err != nil || got["ready"] != true || calls != 2 {
		t.Fatalf("calls=%d result=%v err=%v", calls, got, err)
	}
	if paths := fleet.Elements(fleet.Object(got["changeScope"])["paths"]); len(paths) != 4 {
		t.Fatalf("paths=%v", paths)
	}
}
