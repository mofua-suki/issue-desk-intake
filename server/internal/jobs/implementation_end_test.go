package jobs

import (
	"reflect"
	"testing"
)

// The pure classification runs without PostgreSQL, so the end-reason rules stay
// checked even where the database tests are skipped.
func TestImplementationEndClassifiesTerminalReports(t *testing.T) {
	stale := map[string]any{"error": "固定テストが不合格", "testFailures": map[string]any{"complete": true}}
	cases := []struct {
		name               string
		status, holdReason string
		result, view       map[string]any
		want               map[string]any
	}{
		{"merged over stale failure", "merged", "", stale, map[string]any{"implementationUsageStop": true}, map[string]any{"reason": "success", "usageUnknown": false}},
		{"stopped over stale failure", "stopped", "", stale, nil, map[string]any{"reason": "stopped", "usageUnknown": false}},
		{"question", "held", "question", stale, nil, map[string]any{"reason": "question", "usageUnknown": false}},
		{"review hold", "held", "review_held", nil, nil, map[string]any{"reason": "review-hold", "usageUnknown": false}},
		{"prerequisite missing hold", "held", "prerequisite_missing", stale, nil, map[string]any{"reason": "prerequisite", "usageUnknown": false}},
		{"scope wait", "held", "scope_wait", stale, nil, nil},
		{"deletions", "held", "deletions", stale, nil, nil},
		{"running", "running", "", stale, nil, nil},
		{"queued", "queued", "", stale, nil, nil},
		{"usage unknown guard", "failed", "", map[string]any{"error": "累計使用量が未知です。利用上限"}, nil, map[string]any{"reason": "guard", "usageUnknown": true}},
		{"usage limit before timeout", "failed", "", map[string]any{"testTimeout": map[string]any{"complete": true}}, map[string]any{"implementationUsageStop": true}, map[string]any{"reason": "usage-limit", "usageUnknown": false}},
		{"timeout before fixed test", "failed", "", map[string]any{"testTimeout": map[string]any{"complete": true}, "testFailures": map[string]any{"complete": true}}, nil, map[string]any{"reason": "timeout", "usageUnknown": false}},
		{"fixed test before checks", "failed", "", map[string]any{"error": "固定テスト不合格。lint failed"}, nil, map[string]any{"reason": "fixed-test", "usageUnknown": false}},
		{"checks before prerequisite", "failed", "", map[string]any{"error": "typecheck failed"}, map[string]any{"codexDepsUnavailable": true}, map[string]any{"reason": "checks", "usageUnknown": false}},
		{"prerequisite from view", "failed", "", nil, map[string]any{"codexDepsUnavailable": true}, map[string]any{"reason": "prerequisite", "usageUnknown": false}},
		{"prerequisite before guard", "failed", "", map[string]any{"error": "環境が不足しています。ガードが停止"}, nil, map[string]any{"reason": "prerequisite", "usageUnknown": false}},
		{"other guard", "failed", "", map[string]any{"error": "guard rejected"}, nil, map[string]any{"reason": "guard", "usageUnknown": false}},
		{"no inputs", "failed", "", nil, nil, map[string]any{"reason": "unknown", "usageUnknown": false, "unknownCause": "missing", "missingFields": []any{"result.error", "result.testTimeout", "result.testFailures", "view.implementationUsageStop", "view.codexDepsUnavailable"}}},
		{"present but unmatched", "failed", "", map[string]any{"error": "落ちました", "testTimeout": map[string]any{"complete": false}, "testFailures": nil}, map[string]any{"implementationUsageStop": false, "codexDepsUnavailable": float64(0)}, map[string]any{"reason": "unknown", "usageUnknown": false, "unknownCause": "unclassified", "missingFields": []any{"result.testFailures"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ImplementationEnd(tc.status, tc.holdReason, tc.result, tc.view, 7)
			var want map[string]any
			if tc.want != nil {
				want = map[string]any{"version": 1, "generation": int64(7)}
				for k, v := range tc.want {
					want[k] = v
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ImplementationEnd() = %#v, want %#v", got, want)
			}
		})
	}
}
