package tierpolicy

import "testing"

func TestImplementationPolicyPriorityAndMissingFacts(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name  string
		input Input
		want  Decision
	}{
		{"light", Input{ScopeKnown: &yes, HasAcceptance: true, AcceptanceCount: 1}, Decision{"light", "desk", LightReason}},
		{"many conditions", Input{ScopeKnown: &yes, HasAcceptance: true, AcceptanceCount: 100}, Decision{"light", "desk", LightReason}},
		{"disabled", Input{ScopeKnown: &yes, HasAcceptance: true, AcceptanceCount: 1, DisableLight: true}, Decision{"standard", "desk", LightDisabledReason}},
		{"requested", Input{Requested: "light", StallFix: true, ScopeKnown: &no, CrossProject: true, HeavyReason: "理由", DisableLight: true}, Decision{"light", "user", RequestedReason}},
		{"invalid requested", Input{Requested: "invalid", ScopeKnown: &yes, HasAcceptance: true, AcceptanceCount: 1}, Decision{"light", "desk", LightReason}},
		{"stall", Input{StallFix: true, ScopeKnown: &no, CrossProject: true}, Decision{"standard", "desk", StallReason}},
		{"heavy", Input{ScopeKnown: &no, CrossProject: true, HeavyReason: "不可分"}, Decision{"heavy", "desk", CrossProjectReason + "・" + HeavyReasonPrefix + "不可分"}},
		{"unknown scope", Input{ScopeKnown: &no, HasAcceptance: true, AcceptanceCount: 1}, Decision{"standard", "desk", UnknownScopeReason}},
		{"no scope", Input{HasAcceptance: true, AcceptanceCount: 1}, Decision{"standard", "desk", MissingScopeReason}},
		{"no section", Input{ScopeKnown: &yes}, Decision{"standard", "desk", MissingAcceptanceReason}},
		{"empty section", Input{ScopeKnown: &yes, HasAcceptance: true}, Decision{"standard", "desk", EmptyAcceptanceReason}},
		{"missing both", Input{}, Decision{"standard", "desk", MissingScopeReason + "・" + MissingAcceptanceReason}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.input); got != tc.want {
				t.Fatalf("got=%+v want=%+v", got, tc.want)
			}
		})
	}
}
