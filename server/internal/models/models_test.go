package models

import (
	"errors"
	"issue-desk-intake/server/internal/contract"
	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/pgtest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig() map[string]any {
	return map[string]any{"model": "gpt-main", "codexEquivalents": map[string]any{"fable": "gpt-fable"}, "intake": map[string]any{"tiers": map[string]any{"light": map[string]any{"model": "gpt-light", "claudeModel": "haiku", "reasoningEffort": "low"}, "standard": map[string]any{"model": "gpt-standard"}}}}
}
func TestInitialAndRaisedTiersPreserveDecision(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	c := testConfig()
	d := map[string]any{"requestedTier": "light", "consultationProvider": "claude"}
	tier, e := InitialTier(c, d, now)
	if e != nil || tier["level"] != "light" || tier["model"] != "haiku" || tier["source"] != "user" {
		t.Fatal(tier, e)
	}
	d["tier"] = tier
	raised, e := ChangeTier(c, d, "検査の不合格", now.Add(time.Minute), map[string]any{"carried": true})
	if e != nil || raised["level"] != "standard" || raised["source"] != "user" || raised["decidedAt"] != tier["decidedAt"] || len(fleet.List(raised["history"])) != 1 || len(fleet.List(tier["history"])) != 0 {
		t.Fatal(raised, tier, e)
	}
	d["tier"] = raised
	raised, e = ChangeTier(c, d, "再検査", now, nil)
	if e != nil || raised["level"] != "heavy" {
		t.Fatal(raised, e)
	}
	d["tier"] = raised
	if next, e := ChangeTier(c, d, "上限", now, nil); e != nil || next != nil {
		t.Fatal(next, e)
	}
}

// The requester always chooses the consultation tier; the intake never
// decides one by itself (no pending tier, no default heavy).
func TestInitialTierRequiresRequesterChoice(t *testing.T) {
	c := testConfig()
	now := time.Now()
	for _, d := range []map[string]any{{}, {"consultationProvider": "claude"}, {"requestedTier": ""}} {
		tier, e := InitialTier(c, d, now)
		var problem *contract.Problem
		if tier != nil || !errors.As(e, &problem) || problem.Status != 400 || problem.Code != "TIER_REQUIRED" || !strings.Contains(problem.Error(), "--tier") {
			t.Fatalf("tier無しを断らない: %v %v", tier, e)
		}
	}
	if _, e := InitialTier(map[string]any{}, map[string]any{}, now); e == nil {
		t.Fatal("intake.tiers が無いときも tier 無しは断る")
	}
	tier, e := InitialTier(map[string]any{}, map[string]any{"requestedTier": "standard"}, now)
	if e != nil || tier["level"] != "fixed" || tier["pending"] != nil {
		t.Fatal(tier, e)
	}
	tier, e = InitialTier(c, map[string]any{"requestedTier": "standard", "consultationProvider": "claude"}, now)
	if e != nil || tier["level"] != "standard" || tier["source"] != "user" || tier["pending"] != nil {
		t.Fatal(tier, e)
	}
	settings, e := TierSettings(c, map[string]any{"consultationProvider": "claude"}, "standard")
	if e != nil || settings.Model != "fable" || settings.Effort != "high" {
		t.Fatal(settings, e)
	}
	if _, e := InitialTier(c, map[string]any{"requestedTier": "maximum"}, now); e == nil {
		t.Fatal("invalid tier accepted")
	}
	if _, e := TierSettings(c, map[string]any{"consultationProvider": "unknown"}, "light"); e == nil {
		t.Fatal("invalid provider accepted")
	}
}

// A record saved before the classifier was removed may still be pending; it
// keeps its heavy level and is not raised past the top.
func TestLegacyPendingTierRunsHeavy(t *testing.T) {
	c := testConfig()
	d := map[string]any{"tier": map[string]any{"level": "heavy", "source": "default", "pending": true}}
	if next, e := ChangeTier(c, d, "上限", time.Now(), nil); e != nil || next != nil {
		t.Fatal(next, e)
	}
	if next, e := ChangeTier(c, map[string]any{}, "記録なし", time.Now(), nil); e != nil || next != nil {
		t.Fatal("難易度の記録が無い相談を引き上げた", next, e)
	}
}

func TestDisableValidationBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, input := range []map[string]any{{"until": ""}, {"until": nil}, {"until": "2026-11-09T00:00:00Z"}, {"until": "2026-11-01T00:00:00Z"}, {"reason": "x\ny"}} {
		if _, e := ValidateDisable("claude", input, "利用者", now); e == nil {
			t.Fatal(input)
		}
	}
	if _, e := ValidateDisable("claude", map[string]any{"until": "2026-11-02T09:00+09:00"}, "利用者", now); e != nil {
		t.Fatal(e)
	}
	view := OverridesView(testConfig())
	defaults := fleet.Object(view["defaults"])
	review := fleet.Object(defaults["review"])
	if review["mode"] != "opposite" || fleet.Object(review["opposite"])["claude"].(Settings).Provider != "codex" {
		t.Fatal(view)
	}
}

func TestImplementationModelKeepsProviderSpecificTier(t *testing.T) {
	c := testConfig()
	c["implementation"] = map[string]any{"tiers": map[string]any{"light": map[string]any{"model": "small", "claudeModel": "haiku", "reasoningEffort": "low"}}}
	for _, test := range []struct{ Provider, Model string }{{"codex", "small"}, {"claude", "haiku"}, {"antigravity", "gemini-3.8-flash-high"}} {
		setting, e := ImplementationSettings(c, test.Provider, "light")
		if e != nil || setting.Provider != test.Provider || setting.Model != test.Model {
			t.Fatal(setting, e)
		}
	}
	for _, value := range []any{"", 42, "unknown"} {
		if _, e := TierSettings(c, map[string]any{"consultationProvider": value}, "light"); e == nil {
			t.Fatal(value)
		}
	}
}

func assignmentModelConfig(t *testing.T) map[string]any {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	s := fleet.Service{FleetPath: filepath.Join(root, "fleet.json"), AssignmentsPath: filepath.Join(root, "assignments.json")}
	doc, assignments, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("modelsDigest=%s", assignments.Output["modelsDigest"])
	// Match the configuration consumed by the model settings API.
	c := fleet.Object(fleet.Clone(doc.Defaults))
	for key, value := range doc.Hosts["nas"] {
		c[key] = fleet.Clone(value)
	}
	if err := fleet.ApplyModels(c, assignments.Output["models"]); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAssignmentEffectiveModels(t *testing.T) {
	c := assignmentModelConfig(t)
	for _, tc := range []struct{ level, provider, model string }{
		{"standard", "codex", "gpt-6.1-terra"},
		{"standard", "claude", "sonnet"},
		{"heavy", "codex", "gpt-6.2-sol"},
		{"heavy", "claude", "opus"},
	} {
		t.Run("intake/"+tc.level+"/"+tc.provider, func(t *testing.T) {
			got, err := TierSettings(c, map[string]any{"consultationProvider": tc.provider}, tc.level)
			if err != nil || got.Provider != tc.provider || got.Model != tc.model || got.Effort != "medium" {
				t.Fatalf("got %+v, %v; want %s/%s/medium", got, err, tc.provider, tc.model)
			}
		})
	}
	for level, want := range map[string]string{
		"light": "gpt-6.1-terra", "standard": "gpt-6.1-terra", "heavy": "gpt-6.2-sol",
		"filingCheck": "gpt-6.1-terra", "fixed": "gpt-6.2-sol",
	} {
		t.Run("intake/"+level, func(t *testing.T) {
			got, err := TierSettings(c, map[string]any{"consultationProvider": "codex"}, level)
			if err != nil || got.Provider != "codex" || got.Model != want {
				t.Fatalf("got %+v, %v; want codex/%s", got, err, want)
			}
		})
	}
	for level, want := range map[string]string{"light": "gpt-6.1-terra", "standard": "gpt-6.1-terra", "heavy": "gpt-6.2-sol"} {
		t.Run("implementation/"+level, func(t *testing.T) {
			got, err := ImplementationSettings(c, "codex", level)
			if err != nil || got.Provider != "codex" || got.Model != want {
				t.Fatalf("got %+v, %v; want codex/%s", got, err, want)
			}
		})
	}
	defaults := fleet.Object(OverridesView(c)["defaults"])
	review := fleet.Object(fleet.Object(defaults["review"])["opposite"])["claude"].(Settings)
	priority := defaults["priority"].(Settings)
	if review.Provider != "codex" || review.Model != "gpt-6.2-sol" || priority.Provider != "codex" || priority.Model != "gpt-6-luna" {
		t.Fatalf("review=%+v priority=%+v", review, priority)
	}
}

func TestAssignmentExplicitModelsMatchEquivalents(t *testing.T) {
	c := assignmentModelConfig(t)
	table := fleet.Object(c["codexEquivalents"])
	for claude, want := range map[string]string{"sonnet": "gpt-6.1-terra", "opus": "gpt-6.2-sol", "haiku": "gpt-6-luna", "fable": "gpt-6.2-astra"} {
		if table[claude] != want {
			t.Errorf("codexEquivalents.%s=%v; want %s", claude, table[claude], want)
		}
	}
	check := func(path string, entry map[string]any) {
		t.Helper()
		model, claude := fleet.Text(entry["model"]), fleet.Text(entry["claudeModel"])
		if model != "" && claude != "" && model != fleet.Text(table[claude]) {
			t.Errorf("%s.model=%s; codexEquivalents[%s]=%v", path, model, claude, table[claude])
		}
	}
	check("models", c)
	for _, section := range []string{"implementation", "intake"} {
		for level, raw := range fleet.Object(fleet.Object(c[section])["tiers"]) {
			check(section+".tiers."+level, fleet.Object(raw))
		}
	}
}

// The review-note summary keeps its small fixed model after the classifier
// settings were removed, also when a provider is temporarily disabled.
func TestReviewNotesStepUsesFixedSmallModel(t *testing.T) {
	env := pgtest.NewIsolated(t)
	env.DB.SetMaxOpenConns(1)
	if _, err := env.DB.ExecContext(t.Context(), "CREATE TEMP TABLE settings (key text PRIMARY KEY, value jsonb, expires_at timestamptz, updated_at timestamptz, updated_by text)"); err != nil {
		t.Fatal(err)
	}
	c := assignmentModelConfig(t)
	if _, ok := fleet.Object(fleet.Object(c["intake"])["tiers"])["classifier"]; ok {
		t.Fatal("配備する設定に始めの判定のモデルが残っている")
	}
	s := &Service{DB: env.DB}
	for provider, want := range map[string]string{"codex": "gpt-6-luna", "claude": "haiku"} {
		got, err := s.Resolve(t.Context(), c, map[string]any{"consultationProvider": provider}, ReviewNotesStep)
		if err != nil || got.Provider != provider || got.Model != want || got.Effort != "low" || got.Tier != "" {
			t.Fatalf("%s: got %+v, %v; want %s/low", provider, got, err, want)
		}
	}
	if _, err := s.Resolve(t.Context(), c, map[string]any{}, "classifier"); err == nil {
		t.Fatal("廃止した判定の工程を受け付けた")
	}
	if _, err := s.SetDisable(t.Context(), "codex", map[string]any{}, "test", []map[string]any{{"id": "codex", "ready": true}, {"id": "claude", "ready": true}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Resolve(t.Context(), c, map[string]any{"consultationProvider": "codex"}, ReviewNotesStep)
	if err != nil || got.Provider != "claude" || got.Model != "haiku" || got.ProviderDisable == nil {
		t.Fatalf("一時切り替えで %+v, %v", got, err)
	}
}
