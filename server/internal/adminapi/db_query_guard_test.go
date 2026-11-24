package adminapi

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"issue-desk-intake/server/internal/appdb"
	"issue-desk-intake/server/internal/store"
)

func TestDBQueryGuard(t *testing.T) {
	t.Run("rejects", func(t *testing.T) {
		cases := []struct{ name, query string }{
			{"insert", "INSERT INTO jobs(id) VALUES (1)"},
			{"update", "UPDATE jobs SET status='queued'"},
			{"delete", "DELETE FROM jobs"},
			{"merge", "MERGE INTO jobs USING jobs j ON true WHEN MATCHED THEN DELETE"},
			{"create", "CREATE TABLE bad(id int)"},
			{"drop", "DROP TABLE jobs"},
			{"copy", "COPY jobs TO STDOUT"},
			{"select_into", "SELECT * INTO bad FROM jobs"},
			{"multiple", "SELECT 1; SELECT 2"},
			{"extra_semicolon", "SELECT 1;;"},
			{"for_update", "SELECT * FROM jobs FOR UPDATE"},
			{"for_share", "SELECT * FROM jobs FOR SHARE"},
			{"writing_cte", "WITH changed AS (DELETE FROM jobs RETURNING *) SELECT * FROM changed"},
			{"sleep", "SELECT pg_sleep(1)"},
			{"advisory_lock", "SELECT pg_advisory_lock(1)"},
			{"terminate", "SELECT pg_terminate_backend(1)"},
			{"set_config", "SELECT set_config('search_path', 'public', false)"},
			{"nextval", "SELECT nextval('jobs_id_seq')"},
			{"dblink", "SELECT dblink('connection', 'SELECT 1')"},
			{"to_jsonb", "SELECT to_jsonb(jobs) FROM jobs"},
			{"to_json", "SELECT to_json(jobs) FROM jobs"},
			{"row_to_json", "SELECT row_to_json(jobs) FROM jobs"},
			{"catalog", "SELECT * FROM pg_catalog.pg_class"},
			{"information_schema", "SELECT * FROM information_schema.columns"},
			{"pg_identifier", "SELECT * FROM pg_class"},
			{"other_schema", "SELECT * FROM other.jobs"},
			{"quoted_schema_case", `SELECT * FROM "PUBLIC".jobs`},
			{"other_target", "SELECT * FROM issue_desk_app_migration.drafts"},
			{"three_part", "SELECT public.jobs.id FROM public.jobs"},
			{"comma_schema", "SELECT * FROM jobs, other.jobs"},
			{"join_schema", "SELECT * FROM jobs JOIN other.jobs ON true"},
			{"host_image_tokens", "SELECT * FROM host_image_tokens"},
			{"settings", "SELECT value FROM settings"},
			{"requested_token", "SELECT requested_token FROM ssh_tickets"},
			{"quoted_secret", `SELECT "requested_token" FROM "SSH_TICKETS"`},
			{"alias_secret", "SELECT s.* FROM ssh_tickets s"},
			{"row_secret", "SELECT s FROM ssh_tickets s"},
			{"qualified_secret", "SELECT * FROM public.host_image_tokens"},
			{"unicode_string", "SELECT U&'value'"},
			{"unicode_identifier", `SELECT U&"value"`},
			{"unclosed_string", "SELECT 'value"},
			{"unclosed_comment", "SELECT 1 /* hidden"},
			{"unclosed_nested_comment", "SELECT 1 /* outer /* inner */"},
			{"unclosed_identifier", `SELECT "jobs`},
			{"backslash_string", `SELECT 'a\' DELETE FROM jobs`},
			{"escape_string", `SELECT E'a\'value'`},
			{"dollar_string", "SELECT $$value$$"},
			{"unknown_function", "SELECT user_function()"},
			{"unreserved_filter_function", "SELECT filter()"},
			{"unreserved_over_function", "SELECT over()"},
			{"unreserved_type_input", "SELECT escape 'value'"},
			{"quoted_function", `SELECT "count"(*) FROM jobs`},
			{"qualified_function", "SELECT public.count(*) FROM jobs"},
			{"qualified_syntax", "SELECT public.coalesce(1, 2)"},
			{"operator_function", "SELECT 1 OPERATOR(public.+) 2"},
			{"custom_operator", "SELECT 1 @ 2"},
			{"cast", "SELECT 'settings'::regclass"},
			{"cast_function", "SELECT CAST('settings' AS regclass)"},
			{"type_input", "SELECT unsafe_type 'value'"},
			{"quoted_type_input", `SELECT "unsafe_type" 'value'`},
			{"non_ascii_identifier", "SELECT 秘密 FROM jobs"},
			{"long_identifier", "SELECT " + strings.Repeat("x", 64)},
			{"nul", "SELECT 'a\x00b'"},
			{"unbalanced", "SELECT count(*) FROM (SELECT 1"},
			{"negative_depth", "SELECT 1)"},
			{"parameter", "SELECT $1"},
			{"invalid_number", "SELECT 1e+"},
			{"empty", "-- comment only"},
			{"too_long", "SELECT '" + strings.Repeat("x", 65536) + "'"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := guardDBQuery("shared", tc.query)
				if err == nil || strings.TrimSpace(err.Error()) == "" || got != "" {
					t.Fatalf("拒否と理由が必要です: output=%q error=%v", got, err)
				}
				t.Logf("拒否理由: %s", err)
			})
		}
	})
	t.Run("accepts", func(t *testing.T) {
		cases := []struct{ name, query string }{
			{"count", "SELECT count(*) FROM jobs WHERE status = 'queued'"},
			{"with", "WITH queued AS (SELECT id FROM jobs WHERE status = 'queued') SELECT count(*) FROM queued"},
			{"string_keyword", "SELECT 'DELETE'"},
			{"string_quotes", "SELECT 'it''s DELETE; pg_sleep(1)'"},
			{"unicode_value", "SELECT '受付'"},
			{"comment_keyword", "SELECT /* DELETE pg_sleep(1); */ count(*) FROM jobs -- UPDATE"},
			{"nested_comment", "SELECT /* outer /* DROP */ DELETE */ 1"},
			{"semicolon", "SELECT 1;"},
			{"semicolon_comment", "SELECT 1; -- DELETE;"},
			{"quoted_identifiers", `SELECT "status" FROM "public"."jobs"`},
			{"qualified_table", "SELECT count(*) FROM public.jobs"},
			{"mixed_case", "SeLeCt COUNT(*) FROM jobs"},
			{"arithmetic", "SELECT 1 + 2 * 3, 1.5e-2, -4"},
			{"subquery", "SELECT * FROM (SELECT 1 AS n) q WHERE n IN (1, 2)"},
			{"safe_functions", "SELECT coalesce(max(id), 0) FROM jobs"},
			{"alias", "SELECT status AS state FROM jobs j"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := guardDBQuery("shared", tc.query)
				if err != nil || got == "" {
					t.Fatalf("受付が必要です: output=%q error=%v", got, err)
				}
			})
		}
	})
	t.Run("targets", func(t *testing.T) {
		if _, err := guardDBQuery("app", "SELECT count(*) FROM issue_desk_app_migration.drafts"); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ target, query string }{
			{"app", "SELECT * FROM public.jobs"},
			{"other", "SELECT 1"},
			{"", "SELECT 1"},
		} {
			if _, err := guardDBQuery(tc.target, tc.query); err == nil {
				t.Fatalf("対象外を受け付けました: %s", tc.target)
			}
		}
	})
}

// Exact shipping schema names in a disposable database prevent accidental
// dependence on fixture-only DDL, search_path or another package's test data.
func newDBQueryDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("scripts/with-test-postgres.ts または scripts/verify.sh の一時PostgreSQLが必要です（SKIP不可）")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasPrefix(config.Database, "issue_desk_test") {
		t.Fatal("専用のissue_desk_testデータベースが必要です")
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { admin.Close() })
	name := "issue_desk_test_guard_" + strings.ReplaceAll(randomID(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec("CREATE DATABASE " + quoted + " TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE " + quoted); err != nil {
			t.Error(err)
		}
	})
	config.Database = name
	config.RuntimeParams["search_path"] = "public"
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := appdb.CreateSchema(ctx, tx, "issue_desk_app_migration"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return db
}

// These exact columns contain identifiers/labels, not authentication material.
// The inventory test below requires a decision for each newly introduced matching name.
const dbQueryPublicColumns = `
shared.usage_rows.key shared.usage_limit_rows.key shared.fixed_test_usage.key shared.runner_log_lines.key
app.attention_cases.key app.job_filings.key app.stall_fixes.key
app.draft_withdrawals.token_name app.topic_link_removals.token_name`

// dbQuerySecretColumn classifies one inventory column with the guard's lists.
func dbQuerySecretColumn(target, table, column string) bool {
	return dbQueryListed(dbQueryDeniedTables, table) || dbQueryListed(dbQueryDeniedColumns, column) ||
		(dbQueryCredentialName(column) && !dbQueryListed(dbQueryPublicColumns, target+"."+table+"."+column))
}

func checkDBQueryInventory(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var problems []string
	rows, err := db.Query(`SELECT table_schema, table_name, column_name
FROM information_schema.columns
WHERE table_schema IN ('public', 'issue_desk_app_migration')
AND column_name ~* 'token|hash|secret|key|password|credential|csrf|session'
ORDER BY table_schema, table_name, ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var schema, table, column string
		if err := rows.Scan(&schema, &table, &column); err != nil {
			t.Fatal(err)
		}
		target := "shared"
		if schema == "issue_desk_app_migration" {
			target = "app"
		}
		name := target + "." + table + "." + column
		denied := dbQuerySecretColumn(target, table, column)
		// Unknown credential-like columns fail even though the runtime
		// classifier is conservative: every column needs an explicit review.
		tableDeny := dbQueryListed(dbQueryDeniedTables, table)
		columnDeny := dbQueryListed(dbQueryDeniedColumns, column)
		explicitDeny := tableDeny || columnDeny
		public := dbQueryListed(dbQueryPublicColumns, name)
		if !tableDeny && columnDeny && !public {
			problems = append(problems, fmt.Sprintf("列名だけで拒否されている資格情報らしい列: %s", name))
		} else if !explicitDeny && !public {
			problems = append(problems, fmt.Sprintf("未分類の資格情報らしい列: %s", name))
		}
		if denied == public {
			problems = append(problems, fmt.Sprintf("拒否と公開の分類が矛盾しています: %s", name))
		}
		query := "SELECT " + pgx.Identifier{column}.Sanitize() + " FROM " + pgx.Identifier{schema, table}.Sanitize()
		guarded, guardErr := guardDBQuery(target, query)
		if explicitDeny {
			if guardErr == nil || guarded != "" {
				problems = append(problems, fmt.Sprintf("在庫の拒否列を参照できました: %s", name))
			}
		} else if public {
			if guardErr != nil {
				problems = append(problems, fmt.Sprintf("公開列が拒否されました: %s: %v", name, guardErr))
			} else if result, err := db.Query(guarded); err != nil {
				problems = append(problems, fmt.Sprintf("公開列の実参照に失敗: %s: %v", name, err))
			} else {
				result.Close()
			}
		}
		t.Logf("%s: denied=%t public=%t", name, explicitDeny, public)
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("出荷スキーマの資格情報らしい列が見つかりません")
	}
	return problems
}

func TestDBQuerySecrets(t *testing.T) {
	t.Run("references", testDBQuerySecretReferences)
	t.Run("inventory", func(t *testing.T) {
		db := newDBQueryDatabase(t)
		for _, problem := range checkDBQueryInventory(t, db) {
			t.Error(problem)
		}
	})
	t.Run("inventory_rejects_column_only_deny", func(t *testing.T) {
		db := newDBQueryDatabase(t)
		if _, err := db.Exec(`CREATE TABLE inventory_probe (hash text)`); err != nil {
			t.Fatal(err)
		}
		problems := checkDBQueryInventory(t, db)
		want := "列名だけで拒否されている資格情報らしい列: shared.inventory_probe.hash"
		if len(problems) != 1 || problems[0] != want {
			t.Fatalf("列名だけの拒否を検出できません: %v", problems)
		}
	})
}

// Exercise returned SQL, not just token decisions, against the shipping DDL.
func TestDBQueryPostgresGuardNormalization(t *testing.T) {
	db := newDBQueryDatabase(t)
	if _, err := db.Exec("INSERT INTO jobs(repo,issue,title) VALUES('test/repo',1,'guard')"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SELECT count(*) FROM jobs WHERE status = 'queued'",
		"WITH queued AS (SELECT id FROM jobs) SELECT count(*) FROM queued",
		"SELECT /* DELETE /* nested */ */ count(*) FROM public.jobs; -- trailing",
		"SELECT count(*) FROM jobs WHERE 'DELETE' = 'DELETE'",
	} {
		guarded, err := guardDBQuery("shared", query)
		if err != nil {
			t.Fatal(err)
		}
		var got int
		if err := db.QueryRow("SELECT * FROM (" + guarded + ") q LIMIT 2").Scan(&got); err != nil || got != 1 {
			t.Fatalf("検査後のSQL実行: count=%d error=%v", got, err)
		}
	}
	for _, table := range []string{"settings", "api_tokens", "api_token_grants", "cli_keys", "temporary_code_records"} {
		for _, projection := range []string{"*", "s", "s.*"} {
			if _, err := guardDBQuery("app", "SELECT "+projection+" FROM issue_desk_app_migration."+table+" s"); err == nil {
				t.Errorf("出荷スキーマの資格情報の表を参照できました: %s %s", table, projection)
			}
		}
	}
}
