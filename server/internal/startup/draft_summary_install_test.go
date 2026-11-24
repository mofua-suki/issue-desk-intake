package startup

import (
	"context"
	"testing"

	"issue-desk-intake/server/internal/pgtest"
)

func TestDraftSummaryProductionInstall(t *testing.T) {
	env := pgtest.NewIsolated(t)
	_, err := env.DB.Exec(`DROP TRIGGER drafts_screen_summary ON drafts; DROP FUNCTION ` + env.Schema + `.drafts_set_screen_summary(); DROP FUNCTION ` + env.Schema + `.drafts_screen_summary(text); ALTER TABLE drafts DROP COLUMN screen_summary; INSERT INTO drafts(id,json) VALUES ('legacy','{"title":"old"}')`)
	if err != nil {
		t.Fatal(err)
	}
	config := &Config{AppPostgresDSN: env.DSN, ApplicationSchema: env.Schema}
	for i := 0; i < 2; i++ {
		s, err := openApplicationStore(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		var ok bool
		err = s.DB().QueryRow(`SELECT screen_summary IS NOT NULL AND screen_summary=` + env.Schema + `.drafts_screen_summary(json) FROM drafts WHERE id='legacy'`).Scan(&ok)
		if err != nil || !ok {
			t.Fatalf("startup backfill: %v %v", ok, err)
		}
		_, err = s.DB().Exec(`UPDATE drafts SET json='{"title":"new"}' WHERE id='legacy'`)
		if err != nil {
			t.Fatal(err)
		}
		err = s.DB().QueryRow(`SELECT screen_summary = ` + env.Schema + `.drafts_screen_summary(json) AND screen_summary::json->>'title'='new' FROM drafts WHERE id='legacy'`).Scan(&ok)
		s.Close()
		if err != nil || !ok {
			t.Fatalf("trigger: %v %v", ok, err)
		}
	}
}
