package appsql

import "testing"

func TestActiveDefaultsToPostgres(t *testing.T) {
	if CurrentDialect() != Postgres {
		t.Fatal("default must be PostgreSQL")
	}
	var a Active
	got, _ := a.Bind("SELECT ?, ?")
	if got != "SELECT $1, $2" {
		t.Fatalf("postgres bind = %q", got)
	}
}

func TestSetActiveRejectsUnknown(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	SetActive(Dialect(99))
}

func TestActiveMustHelpersMatchCheckedForms(t *testing.T) {
	var a Active
	bound, _ := a.Bind("SELECT ? WHERE x=?")
	order, _ := a.RowOrder(true)
	insert, _ := a.InsertIgnore("t", []string{"a", "b"}, "VALUES(?,?)")
	exists, _ := a.TableExists()
	text, _ := a.JSONText("json", "$.a.b")
	number, _ := a.JSONNumber("json", "$.n")
	identity, _ := a.Identity()
	binary, _ := a.Binary()
	collation, _ := a.BinaryCollation()
	for name, pair := range map[string][2]string{
		"bind":      {a.MustBind("SELECT ? WHERE x=?"), bound},
		"order":     {a.MustRowOrder(true), order},
		"insert":    {a.MustInsertIgnore("t", []string{"a", "b"}, "VALUES(?,?)"), insert},
		"exists":    {a.MustTableExists(), exists},
		"json":      {a.MustJSONText("json", "$.a.b"), text},
		"number":    {a.MustJSONNumber("json", "$.n"), number},
		"identity":  {a.MustIdentity(), identity},
		"binary":    {a.MustBinary(), binary},
		"collation": {a.MustBinaryCollation(), collation},
	} {
		if pair[0] != pair[1] || pair[0] == "" {
			t.Fatalf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
}

func TestActiveMustHelpersPanicOnInvalidInput(t *testing.T) {
	var a Active
	for name, call := range map[string]func(){
		"bind":   func() { a.MustBind("SELECT ?, $1") },
		"insert": func() { a.MustInsertIgnore("bad name", []string{"a"}, "VALUES(?)") },
		"json":   func() { a.MustJSONText("json", "a.b") },
		"number": func() { a.MustJSONNumber("json", "a.b") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s: expected panic", name)
				}
			}()
			call()
		}()
	}
}
