package appdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"issue-desk-intake/server/internal/draftsummary"
	"issue-desk-intake/server/internal/topicdb"
)

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// CreateSchema creates (idempotently) the complete application schema in the
// PostgreSQL schema named schema: every table of tables, the shared
// source_rowid sequence, the identity column and the indexes. Only the test
// helper internal/pgtest calls it (with a throwaway schema name); no production
// path does.
// schema must be a plain lower-case identifier.
func CreateSchema(ctx context.Context, p *sql.Tx, schema string) error {
	if !schemaName.MatchString(schema) {
		return errors.New("schema name is invalid")
	}
	if _, e := p.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+schema); e != nil {
		return errors.New("schema creation failed")
	}
	if e := prepareSequences(ctx, p, schema); e != nil {
		return e
	}
	for _, t := range tables {
		if _, e := p.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+schema+`."`+t.name+`" (`+sourceRowidColumn(schema)+`,`+t.definition+`)`); e != nil {
			return fmt.Errorf("destination schema failed for %s", t.name)
		}
		if e := upgradeTable(ctx, p, t, schema); e != nil {
			return e
		}
	}
	if err := draftsummary.EnsureDraftSummary(ctx, p, schema); err != nil {
		return err
	}
	if err := topicdb.InstallTopicSummary(ctx, p, schema); err != nil {
		return err
	}
	for _, index := range indexes {
		if _, e := p.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS "idx_`+strings.Fields(index)[0]+`" ON `+schema+`.`+index); e != nil {
			return errors.New("destination index creation failed")
		}
	}
	return nil
}

// TableNames lists the application tables CreateSchema creates (for tests that
// check a schema is complete).
func TableNames() []string {
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.name
	}
	return names
}
