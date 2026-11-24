package usage

import "testing"

func TestMissingReasonsAllowedKeysAndLimit(t *testing.T) {
	reasons := make(map[string]any)
	for _, name := range schema["promptPartNames"] {
		reasons[name] = "input_unavailable"
	}
	for _, name := range []string{"calls", "contextMaxTokens", "contextTotalTokens", "promptTotalBytes", "contextReads"} {
		reasons[name] = "input_unavailable"
	}
	row := map[string]any{"at": "2026-11-03T00:00:00.000Z", "host": "nas", "source": "intake", "measurementMissingReasons": reasons}
	if len(reasons) > MaxMeasurementMissingReasons || !ValidUsage(row) {
		t.Fatalf("許可された欠測理由を受け付けない: %d件、上限%d件", len(reasons), MaxMeasurementMissingReasons)
	}
	reasons["unknown"] = "input_unavailable"
	if ValidUsage(row) {
		t.Fatal("不正な欠測理由のキーを受け付けた")
	}
}
