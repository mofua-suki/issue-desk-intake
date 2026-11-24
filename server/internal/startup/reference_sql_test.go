package startup

import (
	"testing"

	"issue-desk-intake/server/internal/pgtest"
)

// The reference acceptance reads the newest attention case of an issue with these
// statements; run them on PostgreSQL itself.
func TestReferenceSQLRunsOnPostgres(t *testing.T) {
	db := pgtest.New(t)
	var exists int
	if err := db.QueryRow(referenceTableExistsSQL(), "attention_cases").Scan(&exists); err != nil || exists != 1 {
		t.Fatal("表の有無を確かめられません", exists, err)
	}
	if err := db.QueryRow(referenceTableExistsSQL(), "no_such_table").Scan(&exists); err != nil || exists != 0 {
		t.Fatal("無い表を有ると判定", exists, err)
	}
	for _, key := range []string{"older", "newer"} {
		if _, err := db.Exec("INSERT INTO attention_cases(key,json) VALUES(?,?)", key, `{"project":"owner/repo","number":7,"mark":"`+key+`"}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO attention_cases(key,json) VALUES(?,?)", "other", `{"project":"owner/repo","number":8,"mark":"other"}`); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.QueryRow(referenceAttentionSQL(), "owner/repo", 7).Scan(&raw); err != nil || raw != `{"project":"owner/repo","number":7,"mark":"newer"}` {
		t.Fatal("最新の注意事例を読めません", raw, err)
	}

	if _, err := db.Exec("INSERT INTO attention_cases(key,json) VALUES(?,?)", "foreign", `{"project":"owner/other","number":7,"mark":"foreign"}`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(referenceAttentionForIssuesSQL(), "owner/repo", "[7,8,9,7]")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int64]string{}
	for rows.Next() {
		var number int64
		var raw string
		if err := rows.Scan(&number, &raw); err != nil {
			t.Fatal(err)
		}
		got[number] = raw
	}
	if rows.Err() != nil || len(got) != 2 || got[7] != `{"project":"owner/repo","number":7,"mark":"newer"}` || got[8] != `{"project":"owner/repo","number":8,"mark":"other"}` {
		t.Fatal(got, rows.Err())
	}
}
