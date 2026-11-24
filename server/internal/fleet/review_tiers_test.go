package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReviewTiersValidationAndDistribution(t *testing.T) {
	for _, tier := range []string{"light", "standard", "heavy"} {
		for _, tc := range []struct {
			name, field string
			value       any
			valid       bool
		}{
			{"model", "model", " gpt-test ", true}, {"claude", "claudeModel", " sonnet ", true}, {"effort", "reasoningEffort", "high", true},
			{"unknown field", "provider", "claude", false}, {"model type", "model", true, false}, {"empty model", "model", " ", false},
			{"claude type", "claudeModel", 1, false}, {"empty claude", "claudeModel", "", false}, {"effort type", "reasoningEffort", 1, false}, {"effort value", "reasoningEffort", "infinite", false},
		} {
			t.Run(tier+"/"+tc.name, func(t *testing.T) {
				raw := assignmentsBody(t)
				raw["models"] = map[string]any{"review": map[string]any{"tiers": map[string]any{tier: map[string]any{tc.field: tc.value}}}}
				a, err := NormalizeAssignments(raw)
				if (err == nil) != tc.valid {
					t.Fatalf("validation: %v", err)
				}
				config := map[string]any{}
				if !tc.valid {
					if err := applyModels(config, raw["models"]); err == nil {
						t.Fatal("distribution accepted invalid models")
					}
					return
				}
				if err := applyModels(config, a.Output["models"]); err != nil {
					t.Fatal(err)
				}
				got := Object(Object(Object(config["review"])["tiers"])[tier])[tc.field]
				want := tc.value
				if tc.field == "model" {
					want = "gpt-test"
				}
				if tc.field == "claudeModel" {
					want = "sonnet"
				}
				if got != want {
					t.Fatalf("got %v, want %v", got, want)
				}
			})
		}
	}
	for _, tc := range []struct {
		name   string
		review any
	}{
		{"unknown tier", map[string]any{"tiers": map[string]any{"other": map[string]any{}}}},
		{"tiers type", map[string]any{"tiers": []any{}}}, {"tier type", map[string]any{"tiers": map[string]any{"light": "sonnet"}}},
		{"review type", "opus"}, {"null tiers", map[string]any{"tiers": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := assignmentsBody(t)
			raw["models"] = map[string]any{"review": tc.review}
			if _, err := NormalizeAssignments(raw); err == nil {
				t.Fatal("invalid models accepted")
			}
			if err := applyModels(map[string]any{}, raw["models"]); err == nil {
				t.Fatal("invalid models distributed")
			}
		})
	}
}

func TestReviewTiersPartialAndEmptyDistribution(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tiers map[string]any
		want  map[string]any
	}{
		{"partial", map[string]any{"standard": map[string]any{"model": "configured", "reasoningEffort": "max"}}, map[string]any{"standard": map[string]any{"model": "configured", "reasoningEffort": "max"}}},
		{"empty", map[string]any{}, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := assignmentsBody(t)
			raw["models"] = map[string]any{"review": map[string]any{"tiers": tc.tiers}}
			a, err := NormalizeAssignments(raw)
			if err != nil {
				t.Fatal(err)
			}
			config := map[string]any{"review": map[string]any{"tiers": map[string]any{"light": map[string]any{"model": "old"}, "heavy": map[string]any{"model": "old"}}}}
			if err := applyModels(config, a.Output["models"]); err != nil {
				t.Fatal(err)
			}
			if got := Object(config["review"])["tiers"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("tiers: %#v", got)
			}
		})
	}
}

func TestReviewTiersAbsentPreservesLegacyDistribution(t *testing.T) {
	for _, tc := range []struct {
		name   string
		models any
	}{
		{"nil", nil}, {"no review", map[string]any{}}, {"review without tiers", map[string]any{"review": map[string]any{"claudeModel": "opus"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := map[string]any{"review": map[string]any{"provider": "codex", "claudeModel": "old", "tiers": map[string]any{"light": map[string]any{"claudeModel": "local"}}}}
			if err := applyModels(config, tc.models); err != nil {
				t.Fatal(err)
			}
			want := "old"
			if tc.name == "no review" {
				want = ""
			}
			if tc.name == "review without tiers" {
				want = "opus"
			}
			review := Object(config["review"])
			if review["provider"] != "opposite" || Text(review["claudeModel"]) != want || !reflect.DeepEqual(review["tiers"], map[string]any{"light": map[string]any{"claudeModel": "local"}}) {
				t.Fatalf("legacy review changed: %#v", review)
			}
		})
	}
}

func TestShippedAssignmentsDistributeReviewTiers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "assignments.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	a, err := NormalizeAssignments(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"provider": "opposite", "claudeModel": "opus", "tiers": map[string]any{"light": map[string]any{"claudeModel": "sonnet"}, "standard": map[string]any{"claudeModel": "sonnet"}, "heavy": map[string]any{"claudeModel": "opus"}}}
	config := map[string]any{}
	if err := applyModels(config, a.Output["models"]); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config["review"], want) {
		t.Fatalf("review: %#v", config["review"])
	}
	// Load both shipped files using the production service, then apply the normalized models.
	_, loaded, err := (&Service{FleetPath: filepath.Join("..", "..", "..", "fleet.json"), AssignmentsPath: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	config = map[string]any{}
	if err := applyModels(config, loaded.Output["models"]); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config["review"], want) {
		t.Fatalf("loaded review: %#v", config["review"])
	}
}
