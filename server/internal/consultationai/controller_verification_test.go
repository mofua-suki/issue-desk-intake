package consultationai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/ai"
	"issue-desk-intake/server/internal/fleet"
)

func TestControllerGateDoesNotDuplicateImplementationCapabilities(t *testing.T) {
	seen := false
	s, d := testService(t, func(_ context.Context, r ai.Request) (ai.Answer, error) {
		_, payload, ok := strings.Cut(r.Input, "以下は相談と資料のデータです:\n")
		var data Record
		if !ok || json.Unmarshal([]byte(payload), &data) != nil {
			t.Fatal("missing structured consultation input")
		}
		gate := fleet.Object(data["controllerVerification"])
		value := gateReply()
		requirement := fleet.Object(fleet.Elements(fleet.Object(value["executionPlan"])["requirements"])[0])
		if gate["fixedGateOwner"] == "controller" && gate["duplicateImplementerRequirement"] == false && gate["successRequiredBeforeMerge"] == true && gate["preserveExplicitAcceptanceChecks"] == true {
			seen = true
			requirement["capabilities"] = []any{}
		} else {
			requirement["capabilities"] = []any{"gcc", "cgo", "race"}
		}
		return measuredAnswer(value), nil
	})
	s.options.Environment = func(context.Context, string) ([]EnvironmentHost, error) {
		return []EnvironmentHost{{Name: "pc", Values: map[string]any{"executionPlanVersion": float64(1), "gcc": false, "cgo": false, "race": false}}}, nil
	}
	result, err := s.Turn(context.Background(), d)
	if err != nil || !seen || result["ready"] != true {
		t.Fatalf("controller verification was assigned to implementation: seen=%v result=%v err=%v", seen, result, err)
	}
	requirements := fleet.Elements(fleet.Object(result["executionPlan"])["requirements"])
	if len(requirements) != 1 || len(fleet.Elements(fleet.Object(requirements[0])["capabilities"])) != 0 {
		t.Fatal("individual acceptance check changed", requirements)
	}
	plan := fleet.Object(result["completionPlan"])
	if len(fleet.Elements(plan["steps"])) != 4 {
		t.Fatal("completion verification gate missing", plan)
	}
}
