package testcause

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestReportRevalidatesProofsAndPreservesPublicClassification(t *testing.T) {
	raw, e := os.ReadFile("testdata/report.json")
	if e != nil {
		t.Fatal(e)
	}
	var cases []struct {
		Input    Object
		Expected Object
		Budget   int
	}
	if e = json.Unmarshal(raw, &cases); e != nil {
		t.Fatal(e)
	}
	for index, item := range cases {
		budget := item.Budget
		if budget == 0 {
			budget = 24576
		}
		result := Report(item.Input, budget)
		encoded, _ := json.Marshal(result)
		var got Object
		_ = json.Unmarshal(encoded, &got)
		if !reflect.DeepEqual(got, item.Expected) {
			t.Errorf("case %d got=%s expected=%s", index, encoded, encoding(item.Expected))
		}
	}
}
