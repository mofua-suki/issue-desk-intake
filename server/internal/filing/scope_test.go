package filing

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBackfillPreservesTrailerAndRejectsQuotedScope(t *testing.T) {
	trailer := "\n\n---\n受付: 開発受付 / owner/app\n\n<!-- issue-desk:11111111-1111-4111-8111-111111111111 -->"
	body := "依頼" + scopeSection(Record{"known": false, "paths": []any{}, "reason": "未確認"}) + trailer
	next := BackfilledBody(body, Record{"known": true, "paths": []any{"a.go"}, "reason": ""})
	parsed := ParseScope(next)
	if next == "" || parsed.State != "declared" || !parsed.Backfilled || len(parsed.Paths) != 1 || parsed.Paths[0] != "a.go" || !strings.HasSuffix(next, trailer) {
		t.Fatalf("%+v %s", parsed, next)
	}
	if BackfilledBody(next, Record{"known": true, "paths": []any{"b.go"}}) != "" {
		t.Fatal("declared scope replaced")
	}
	quoted := "引用 <!-- issue-desk-scope:{\"v\":1,\"paths\":[\"x.go\"]} -->\n後続" + trailer
	if ParseScope(quoted).State != "absent" {
		t.Fatal("quoted marker accepted")
	}
}
func TestPendingRoundTripKeepsFieldsOwnedByOtherComponents(t *testing.T) {
	raw := []byte(`{"repo":"owner/app","number":2,"draftId":"id","filingGroup":{"family":"root","owner":"token:internal"},"kind":"followup","future":{"keep":true}}`)
	var e pending
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	e.Estimating = true
	encoded, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	json.Unmarshal(encoded, &saved)
	if saved["future"] == nil || saved["filingGroup"] == nil || saved["kind"] != "followup" || saved["estimating"] != true {
		t.Fatal("unowned metadata lost", string(encoded))
	}
}
