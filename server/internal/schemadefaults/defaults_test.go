package schemadefaults_test

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"

	"issue-desk-intake/server/internal/appdb"
	"issue-desk-intake/server/internal/pgtest"
	"issue-desk-intake/server/internal/schemadefaults"
)

// appInserts holds, for every application table, the INSERT the application
// runs (same column list, columns left out exactly as in the code: grep
// "INSERT INTO <table>" under server/internal). Tables nothing in Go writes
// (retired Node tables, written only by the migration) list their NOT NULL
// columns. A table missing here fails TestEveryTableAcceptsTheApplicationInsert,
// so a new table cannot be added without proving its INSERT form works.
var appInserts = map[string]string{
	"settings":               "INSERT INTO settings(id,json) VALUES('k','{}')",
	"drafts":                 "INSERT INTO drafts(id,json) VALUES('k','{}')",
	"todos":                  "INSERT INTO todos(id,json) VALUES('k','{}')",
	"actions":                "INSERT INTO actions(id,json) VALUES('k','{}')",
	"close_requests":         "INSERT INTO close_requests(id,json) VALUES('k','{}')",
	"close_reasons":          "INSERT INTO close_reasons(id,json) VALUES('k','{}')",
	"projects":               "INSERT INTO projects(id,json) VALUES('k','{}')",
	"proposals":              "INSERT INTO proposals(id,json) VALUES('k','{}')",
	"proposal_scans":         "INSERT INTO proposal_scans(id,json) VALUES('k','{}')",
	"decisions":              "INSERT INTO decisions(id,json) VALUES('k','{}')",
	"ai_decisions":           "INSERT INTO ai_decisions(id,json) VALUES('k','{}')",
	"intake_error_stalls":    "INSERT INTO intake_error_stalls(id,json) VALUES('k','{}')",
	"attention_cases":        "INSERT INTO attention_cases(key,json) VALUES('k','{}')",
	"job_filings":            "INSERT INTO job_filings(key,json) VALUES('k','{}')",
	"temporary_code_records": "INSERT INTO temporary_code_records(repo,item,stage,json) VALUES('r','i','s','{}')",
	"full_test_intake":       "INSERT INTO full_test_intake(repo,json) VALUES('r','{}')",
	"api_tokens":             "INSERT INTO api_tokens(id,name,hash,issued_at) VALUES('i','n','h','t')",
	"cli_keys":               "INSERT INTO cli_keys(hash,expires) VALUES('h',1)",
	"api_token_grants":       "INSERT INTO api_token_grants(token_id,granted_at) VALUES('i','t')",
	"admin_audit":            "INSERT INTO admin_audit(at,actor,action,target) VALUES('t','a','x','y')",
	"images":                 "INSERT INTO images(id,scope,message,json,bytes) VALUES('i','s','m','{}','\\x00')",
	"intake_receipts":        "INSERT INTO intake_receipts(id,sender,send_id,json) VALUES('i','s','d','{}')",
	"draft_withdrawals":      "INSERT INTO draft_withdrawals(draft,project,title,operation,at,created_at,via,token_name,reason,parent) VALUES('d','p','t','withdraw','a','c','v',NULL,NULL,NULL)",
	"intake_error_failures":  "INSERT INTO intake_error_failures(draft,gist,at,json) VALUES('d','g','a','{}')",
	"reply_decisions":        "INSERT INTO reply_decisions(id,project,draft,at,json) VALUES('i','p','d','a','{}')",
	"proxy_answers":          "INSERT INTO proxy_answers(id,project,draft,at,status,json) VALUES('i','p','d','a','active','{}')",
	"desk_numbers":           "INSERT INTO desk_numbers(number,project,gitea,assigned_at) VALUES(1,'p',1,'t')",
	"desk_projects":          "INSERT INTO desk_projects(project,scanned_at) VALUES('p','t')",
	"desk_counter":           "INSERT INTO desk_counter(id,next) VALUES(1,2)",
	"usage_review_rounds":    "INSERT INTO usage_review_rounds(round,json) VALUES(1,'{}')",
	"usage_review_acks":      "INSERT INTO usage_review_acks(round,at) VALUES(1,'t')",
	"stall_fixes":            "INSERT INTO stall_fixes(draft,key,stall,filed_at) VALUES('d','k','s','t')",
	"scope_records":          "INSERT INTO scope_records(id,json) VALUES('k','{}')",
	"usage_review_days":      "INSERT INTO usage_review_days(date,json) VALUES('d','{}')",
	"priority_review_days":   "INSERT INTO priority_review_days(date,json) VALUES('d','{}')",
	"inventory_rounds":       "INSERT INTO inventory_rounds(round,project,json) VALUES(1,'p','{}')",
	"priority_review_rounds": "INSERT INTO priority_review_rounds(round,json) VALUES(1,'{}')",
	"main_break_cases":       "INSERT INTO main_break_cases(repo,json) VALUES('r','{}')",
	"topic_link_removals":    "INSERT INTO topic_link_removals(a,b,draft,other,at,token_name) VALUES('a','b','d','o','t',NULL)",
}

// Every application table accepts the INSERT form the application uses; the
// ones that leave a number to the database get a number.
func TestEveryTableAcceptsTheApplicationInsert(t *testing.T) {
	db := pgtest.New(t)
	names := appdb.TableNames()
	sort.Strings(names)
	for _, name := range names {
		q, ok := appInserts[name]
		if !ok {
			t.Errorf("%s: no application INSERT form is recorded in appInserts", name)
			continue
		}
		if _, err := db.Exec(q); err != nil {
			t.Errorf("%s: the application INSERT fails: %v", name, err)
		}
	}
	for name := range appInserts {
		found := false
		for _, n := range names {
			found = found || n == name
		}
		if !found {
			t.Errorf("%s is recorded in appInserts but is not an application table", name)
		}
	}
}

// numbered holds the production tables and their PostgreSQL identity columns
// that number rows when omitted from application INSERTs.
var numbered = map[string]string{"admin_audit": "id", "draft_withdrawals": "sequence", "intake_error_failures": "id"}

func TestNewSchemaNumbersOmittedIdentityColumns(t *testing.T) {
	db := pgtest.New(t)
	for table, column := range numbered {
		var first, second int64
		insert := appInserts[table]
		for i, dst := range []*int64{&first, &second} {
			if err := db.QueryRow(insert + ` RETURNING ` + column).Scan(dst); err != nil {
				t.Fatalf("%s insert %d: %v", table, i, err)
			}
		}
		if first < 1 || second != first+1 {
			t.Errorf("%s: ids %d, %d are not consecutive from 1", table, first, second)
		}
	}
}

// legacy turns the identity columns back into the first PostgreSQL schema's
// plain BIGINT PRIMARY KEY.
func legacy(t *testing.T, env pgtest.Env) {
	t.Helper()
	for table, column := range numbered {
		if _, err := env.DB.Exec(`ALTER TABLE ` + table + ` ALTER COLUMN ` + column + ` DROP IDENTITY`); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if _, err := env.DB.Exec(appInserts[table]); err == nil {
			t.Fatalf("%s: the legacy table must reject an INSERT without its number", table)
		}
	}
}

func TestEnsureDefaultsRepairsLegacyTablesAndContinuesAboveExistingRows(t *testing.T) {
	env := pgtest.NewIsolated(t)
	legacy(t, env)
	for table, column := range numbered {
		for _, id := range []int{3, 9} {
			q := appInserts[table]
			// the existing rows carry their own number
			if _, err := env.DB.Exec(withNumber(t, table, column, q, id)); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
		}
	}
	for run := 1; run <= 2; run++ {
		failed, err := schemadefaults.EnsureDefaults(context.Background(), env.DB, env.Schema)
		if err != nil || len(failed) != 0 {
			t.Fatalf("run %d: %v %v", run, failed, err)
		}
		for table, column := range numbered {
			var id int64
			if err := env.DB.QueryRow(appInserts[table] + ` RETURNING ` + column).Scan(&id); err != nil {
				t.Fatalf("run %d %s: %v", run, table, err)
			}
			// run 1 continues from 9; run 2 changed nothing, so it continues from run 1's row
			if want := int64(9 + run); id != want {
				t.Errorf("run %d %s: got %d, want %d", run, table, id, want)
			}
		}
	}
}

func TestEnsureDefaultsLeavesEmptyTablesStartingAtOne(t *testing.T) {
	env := pgtest.NewIsolated(t)
	legacy(t, env)
	if failed, err := schemadefaults.EnsureDefaults(context.Background(), env.DB, env.Schema); err != nil || len(failed) != 0 {
		t.Fatal(failed, err)
	}
	var id int64
	if err := env.DB.QueryRow(appInserts["admin_audit"] + ` RETURNING id`).Scan(&id); err != nil || id != 1 {
		t.Fatalf("empty table must start at 1: %d %v", id, err)
	}
}

func TestEnsureDefaultsRejectsABadSchemaName(t *testing.T) {
	if _, err := schemadefaults.EnsureDefaults(context.Background(), nil, "x; DROP SCHEMA public"); err == nil {
		t.Fatal("a schema name that is not a plain identifier must be refused")
	}
}

func TestEnsureDefaultsIgnoresAMissingSchema(t *testing.T) {
	env := pgtest.NewIsolated(t)
	if failed, err := schemadefaults.EnsureDefaults(context.Background(), env.DB, "no_such_schema"); err != nil || len(failed) != 0 {
		t.Fatalf("a schema without the tables is not an error: %v %v", failed, err)
	}
}

// withNumber rewrites the recorded INSERT to carry its own number in column.
func withNumber(t *testing.T, table, column, insert string, id int) string {
	t.Helper()
	open := strings.Index(insert, "(")
	values := strings.Index(insert, ") VALUES(")
	if open < 0 || values < 0 {
		t.Fatalf("%s: unexpected INSERT form", table)
	}
	return insert[:open+1] + column + "," + insert[open+1:values+len(") VALUES(")] + strconv.Itoa(id) + "," + insert[values+len(") VALUES("):]
}

// The repair list and the numbered tables this file tests are the same set.
func TestColumnsMatchTheNumberedTables(t *testing.T) {
	if len(schemadefaults.Columns) != len(numbered) {
		t.Fatalf("Columns has %d entries, numbered has %d", len(schemadefaults.Columns), len(numbered))
	}
	for _, c := range schemadefaults.Columns {
		if numbered[c.Table] != c.Column {
			t.Errorf("%s.%s is not in numbered", c.Table, c.Column)
		}
	}
}
