package statistics

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestIssueUsageReportMatchesOriginalFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/issue-usage-report.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Input struct {
			Jobs, Events, Usage []AccountRecord
			Weights, Period     AccountRecord
			Skipped             int
		}
		Report AccountRecord
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	got := BuildIssueUsageReport(ReworkRows{Jobs: f.Input.Jobs, Events: f.Input.Events, Usage: f.Input.Usage, Skipped: f.Input.Skipped}, f.Input.Weights, f.Input.Period, AccountRecord{"draftPolicy": "first-filed-issue", "splitRows": 0})
	if !reflect.DeepEqual(normalized(got), normalized(f.Report)) {
		t.Fatal(jsonDifference(normalized(got), normalized(f.Report), "report"))
	}
}
