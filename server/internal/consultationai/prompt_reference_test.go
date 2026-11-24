package consultationai

import (
	"strings"
	"testing"
)

func TestConsultationInputsDoNotDuplicateLatestReference(t *testing.T) {
	for _, tc := range []struct {
		name   string
		latest string
		count  int
	}{
		{"same", "fixture-current-reference", 1},
		{"different", "fixture-older-reference", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Record{"references": []any{Record{"history": "fixture-current-reference"}}, "messages": []any{Record{"role": "user", "references": []Record{{"history": tc.latest}}}, Record{"role": "assistant", "text": "reply"}}}
			for _, input := range []string{prompt(d, nil, nil, 1, nil, ""), proxyPrompt(nil, nil, d, false, nil, nil, 0)} {
				if strings.Count(input, "fixture-current-reference") != tc.count || !strings.Contains(input, tc.latest) {
					t.Fatal("reference was lost or repeated")
				}
			}
		})
	}
}
