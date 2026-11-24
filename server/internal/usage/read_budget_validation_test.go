package usage

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDeliveryBudgetValidation(t *testing.T) {
	cases := []struct {
		name, budget string
		valid        bool
	}{
		{"absent", "", true}, {"zero", `,"deliveryBudget":{"round":0,"limited":false,"omitted":0,"deliveredBytes":0}`, true},
		{"limits", `,"deliveryBudget":{"round":9007199254740991,"limited":true,"omitted":9007199254740991,"deliveredBytes":160000}`, true},
		{"null", `,"deliveryBudget":null`, false}, {"empty", `,"deliveryBudget":{}`, false},
		{"missing round", `,"deliveryBudget":{"limited":false,"omitted":0,"deliveredBytes":0}`, false},
		{"missing limited", `,"deliveryBudget":{"round":0,"omitted":0,"deliveredBytes":0}`, false},
		{"missing omitted", `,"deliveryBudget":{"round":0,"limited":false,"deliveredBytes":0}`, false},
		{"missing bytes", `,"deliveryBudget":{"round":0,"limited":false,"omitted":0}`, false},
		{"extra", `,"deliveryBudget":{"round":0,"limited":false,"omitted":0,"deliveredBytes":0,"extra":1}`, false},
		{"round negative", `,"deliveryBudget":{"round":-1,"limited":false,"omitted":0,"deliveredBytes":0}`, false},
		{"round unsafe", `,"deliveryBudget":{"round":9007199254740992,"limited":false,"omitted":0,"deliveredBytes":0}`, false},
		{"round fractional", `,"deliveryBudget":{"round":0.5,"limited":false,"omitted":0,"deliveredBytes":0}`, false},
		{"limited type", `,"deliveryBudget":{"round":0,"limited":0,"omitted":0,"deliveredBytes":0}`, false},
		{"omitted negative", `,"deliveryBudget":{"round":0,"limited":false,"omitted":-1,"deliveredBytes":0}`, false},
		{"omitted unsafe", `,"deliveryBudget":{"round":0,"limited":false,"omitted":9007199254740992,"deliveredBytes":0}`, false},
		{"omitted type", `,"deliveryBudget":{"round":0,"limited":false,"omitted":"0","deliveredBytes":0}`, false},
		{"bytes negative", `,"deliveryBudget":{"round":0,"limited":false,"omitted":0,"deliveredBytes":-1}`, false},
		{"bytes over", `,"deliveryBudget":{"round":0,"limited":true,"omitted":0,"deliveredBytes":160001}`, false},
		{"bytes fractional", `,"deliveryBudget":{"round":0,"limited":false,"omitted":0,"deliveredBytes":0.5}`, false},
		{"bytes null", `,"deliveryBudget":{"round":0,"limited":false,"omitted":0,"deliveredBytes":null}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"at":"2026-11-01T00:00:00.000Z","host":"nas","source":"intake","contextReads":{"requested":0,"repeated":0,"files":[]` + tc.budget + `}}`
			var row map[string]any
			if err := json.Unmarshal([]byte(raw), &row); err != nil {
				t.Fatal(err)
			}
			if got := ValidUsage(row); got != tc.valid {
				t.Errorf("ValidUsage = %v, want %v", got, tc.valid)
			}
			row["source"] = "worker"
			if ValidUsage(row) {
				t.Error("worker contextReads accepted")
			}
		})
	}
	for _, reads := range []string{`{"requested":0,"repeated":0,"files":[],"extra":0}`, `{"requested":0,"repeated":0,"deliveryBudget":{"round":0,"limited":false,"omitted":0,"deliveredBytes":0}}`} {
		var row map[string]any
		if err := json.Unmarshal([]byte(`{"at":"2026-11-01T00:00:00.000Z","host":"nas","source":"intake","contextReads":`+reads+`}`), &row); err != nil {
			t.Fatal(err)
		}
		if ValidUsage(row) {
			t.Errorf("invalid contextReads accepted: %s", reads)
		}
	}
}

func TestDeliveryBudgetStoreAndRead(t *testing.T) {
	db := migratedUsageDB(t)
	s := Store{DB: db}
	var rows []map[string]any
	raw := `[{"at":"2026-11-01T00:00:00.000Z","host":"nas","source":"intake","contextReads":{"requested":2,"repeated":1,"files":[]}},{"at":"2026-11-01T00:00:01.000Z","host":"nas","source":"intake","contextReads":{"requested":2,"repeated":1,"files":[],"deliveryBudget":{"round":0,"limited":false,"omitted":0,"deliveredBytes":0}}},{"at":"2026-11-01T00:00:02.000Z","host":"nas","source":"intake","contextReads":{"requested":2,"repeated":1,"files":[],"deliveryBudget":{"round":3,"limited":true,"omitted":2,"deliveredBytes":160000}}}]`
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatal(err)
	}
	counts, err := s.AddRows(context.Background(), "test", rows)
	if err != nil || counts != (Counts{Added: 3}) {
		t.Fatalf("AddRows: %+v %v", counts, err)
	}
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got, skipped, err := s.Read(context.Background(), "", from, from.Add(time.Hour))
	if err != nil || skipped != 0 || !reflect.DeepEqual(got, rows) {
		t.Fatalf("Read: %+v skipped=%d err=%v", got, skipped, err)
	}
}
