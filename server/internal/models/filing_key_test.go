package models

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"sync"
	"testing"
	"time"

	"issue-desk-intake/server/internal/fleet"
	"issue-desk-intake/server/internal/usage"
)

type emptySettingsDriver struct{}
type emptySettingsConn struct{}
type emptySettingsRows struct{}

var registerEmptySettingsDriver sync.Once

func (emptySettingsDriver) Open(string) (driver.Conn, error)  { return emptySettingsConn{}, nil }
func (emptySettingsConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (emptySettingsConn) Close() error                        { return nil }
func (emptySettingsConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (emptySettingsConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return emptySettingsRows{}, nil
}
func (emptySettingsRows) Columns() []string         { return []string{"value", "expires_at"} }
func (emptySettingsRows) Close() error              { return nil }
func (emptySettingsRows) Next([]driver.Value) error { return io.EOF }

func TestFilingUsageStepSelectsConfiguredModelAndView(t *testing.T) {
	c := assignmentModelConfig(t)
	registerEmptySettingsDriver.Do(func() { sql.Register("filing-key-test", emptySettingsDriver{}) })
	db, err := sql.Open("filing-key-test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := (&Service{DB: db}).Resolve(context.Background(), c, map[string]any{"consultationProvider": "codex"}, usage.FilingCheckStep)
	if err != nil {
		t.Fatal(err)
	}
	view := fleet.Object(fleet.Object(OverridesView(c)["defaults"])["intake"])
	displayed, ok := view[FilingCheckConfigKey].(Settings)
	if !ok || got.Model != "gpt-6.1-terra" || displayed.Model != got.Model {
		t.Fatalf("resolved=%+v displayed=%+v", got, view[FilingCheckConfigKey])
	}
}

func TestFilingSwitchFixtureSeparatesFallbackModels(t *testing.T) {
	c := assignmentModelConfig(t)
	heavy, err := TierSettings(c, map[string]any{"consultationProvider": "codex"}, "heavy")
	if err != nil {
		t.Fatal(err)
	}
	filing, err := TierSettings(c, map[string]any{"consultationProvider": "codex"}, FilingCheckConfigKey)
	if err != nil {
		t.Fatal(err)
	}
	if heavy.Model != "gpt-6.2-sol" || filing.Model != "gpt-6.1-terra" || heavy.Model == filing.Model {
		t.Fatalf("heavy=%+v filing=%+v", heavy, filing)
	}
}

func TestPostgresFilingCheckSwitchesToConfiguredCodexModel(t *testing.T) {
	db, ctx := testPostgres(t)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s := Service{DB: db, Now: func() time.Time { return now }}
	ready := []map[string]any{{"id": "codex", "ready": true}, {"id": "claude", "ready": true}}
	if _, err := s.SetDisable(ctx, "claude", map[string]any{"reason": "test switch"}, "test", ready); err != nil {
		t.Fatal(err)
	}
	c := assignmentModelConfig(t)
	resolve := func() (Settings, error) {
		return s.Resolve(ctx, c, map[string]any{"consultationProvider": "claude", "tier": map[string]any{"level": "heavy"}}, usage.FilingCheckStep)
	}
	got, err := resolve()
	if err != nil {
		t.Fatal(err)
	}
	view := fleet.Object(fleet.Object(OverridesView(c)["defaults"])["intake"])
	displayed, ok := view[FilingCheckConfigKey].(Settings)
	if !ok || got.Provider != "codex" || got.Model != "gpt-6.1-terra" || got.Model != displayed.Model || got.ProviderDisable["from"] != "claude" || got.ProviderDisable["source"] != "settings" || got.ProviderDisable["reason"] != "test switch" {
		t.Fatalf("resolved=%+v displayed=%+v", got, view[FilingCheckConfigKey])
	}
	// Change the sonnet equivalent to differ from the explicit filing model;
	// the heavy model already differs. This exposes bypasses of the filing switch.
	fleet.Object(c["codexEquivalents"])["sonnet"] = "different-equivalent"
	got, err = resolve()
	if err != nil || got.Provider != "codex" || got.Model != "gpt-6.1-terra" {
		t.Fatalf("filing-specific switch: resolved=%+v err=%v", got, err)
	}
}
