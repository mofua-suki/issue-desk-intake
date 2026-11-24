package usage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"issue-desk-intake/server/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func migratedUsageDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("ISSUE_DESK_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN is not configured")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || !strings.HasPrefix(config.Database, "issue_desk_test") {
		t.Fatal("tests require a dedicated issue_desk_test database")
	}
	ctx := context.Background()
	admin, err := store.OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var database string
	if err := admin.QueryRowContext(ctx, "SELECT current_database()").Scan(&database); err != nil || !strings.HasPrefix(database, "issue_desk_test") {
		admin.Close()
		t.Fatal("test database verification failed")
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	schema := fmt.Sprintf("usage_test_%x", nonce)
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test DB: %v", err)
		}
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("test schema cleanup: %v", err)
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close admin DB: %v", err)
		}
	})
	migrations, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, "SELECT version, name FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatal(err)
		}
		if i >= len(migrations) || version != migrations[i].Version || name != migrations[i].Name {
			t.Fatalf("unexpected migration at index %d: version %d name %q", i, version, name)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if i != len(migrations) {
		t.Fatalf("migration count = %d, want %d", i, len(migrations))
	}
	return db
}

func TestCompactJSONIdentity(t *testing.T) {
	cases := map[string]string{
		`{"z":1,"2":2,"1":3,"a":4,"z":5,"01":6}`:              `{"1":3,"2":2,"z":5,"a":4,"01":6}`,
		`[ -0, 1e-7, 1e-6, 1e20, 1e21, "<>&\u2028\u2029\n" ]`: "[0,1e-7,0.000001,100000000000000000000,1e+21,\"<>&\u2028\u2029\\n\"]",
	}
	for raw, want := range cases {
		got, err := Compact([]byte(raw))
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	for _, raw := range []string{`{} {}`, `[1,]`, `{"x":`} {
		if _, err := Compact([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestFirstOutputMsValidation(t *testing.T) {
	if !ValidUsage(map[string]any{"at": "2026-11-01T00:00:00.000Z", "source": "intake", "host": "nas"}) {
		t.Fatal("absent firstOutputMs rejected")
	}
	for _, tc := range []struct {
		name, source string
		value        any
		valid        bool
	}{
		{"positive", "intake", float64(123), true},
		{"zero", "intake", float64(0), true},
		{"maximum", "intake", float64(9007199254740991), true},
		{"negative", "intake", float64(-1), false},
		{"fraction", "intake", 1.5, false},
		{"out of range", "intake", float64(9007199254740992), false},
		{"string", "intake", "1", false},
		{"boolean", "intake", true, false},
		{"null", "intake", nil, false},
		{"worker", "worker", float64(1), false},
		{"worker null", "worker", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := map[string]any{"at": "2026-11-01T00:00:00.000Z", "source": tc.source, "host": "nas", "firstOutputMs": tc.value}
			if got := ValidUsage(row); got != tc.valid {
				t.Fatalf("ValidUsage = %v, want %v", got, tc.valid)
			}
		})
	}
}

func TestFirstOutputMsStoreAndRead(t *testing.T) {
	db := migratedUsageDB(t)
	ctx := context.Background()
	usageStore := Store{DB: db}
	rows := []map[string]any{
		{"at": "2026-11-01T00:00:00.000Z", "source": "intake", "host": "nas", "firstOutputMs": 123},
		{"at": "2026-11-01T00:00:01.000Z", "source": "intake", "host": "nas", "firstOutputMs": 0},
		{"at": "2026-11-01T00:00:02.000Z", "source": "intake", "host": "nas"},
		{"at": "2026-11-01T00:00:03.000Z", "source": "worker", "host": "nas"},
	}
	counts, err := usageStore.AddRows(ctx, "test", rows)
	if err != nil || counts.Added != len(rows) || counts.Rejected != 0 {
		t.Fatalf("AddRows = %+v, %v", counts, err)
	}
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got, skipped, err := usageStore.Read(ctx, "", from, from.Add(time.Hour))
	if err != nil || skipped != 0 || len(got) != len(rows) {
		t.Fatalf("Read = %+v, skipped %d, %v", got, skipped, err)
	}
	for i, want := range []any{float64(123), float64(0), nil, nil} {
		value, exists := got[i]["firstOutputMs"]
		if value != want || exists != (i < 2) {
			t.Errorf("row %d firstOutputMs = %v (exists %v), want %v", i, value, exists, want)
		}
	}
}

func TestUsageRejectsContentAndKeepsMissingMeasurements(t *testing.T) {
	var row map[string]any
	if err := json.Unmarshal([]byte(`{"at":"2026-11-01T00:00:00.000Z","source":"worker","host":"nas","repo":"owner/repo","issue":1,"provider":"codex","inputTokens":null,"outputTokens":0,"usageMissingReason":"usage-unavailable","dispatchGeneration":0,"leaseEpoch":1}`), &row); err != nil {
		t.Fatal(err)
	}
	if !ValidUsage(row) {
		t.Fatal("valid missing measurement rejected")
	}
	row["prompt"] = "private content"
	if ValidUsage(row) {
		t.Fatal("accepted prompt")
	}
	delete(row, "prompt")
	row["files"] = []any{map[string]any{"path": "a.go", "prompt": "private content"}}
	if ValidUsage(row) {
		t.Fatal("accepted nested prompt")
	}
	delete(row, "files")
	row["inputTokens"] = -1.0
	if ValidUsage(row) {
		t.Fatal("accepted negative tokens")
	}
}

func TestBillingValidationAndLegacyIdentity(t *testing.T) {
	legacy := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(`{"at":"2026-11-01T00:00:00.000Z","host":"nas","source":"worker","provider":"claude"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	stored, err := OrderedRow(legacy, schema["storageFields"])
	if err != nil || hash(stored) != "20a6062e8ef4b84bcadb54f44f220c3dd84eec22693e78425bd0826b318123fc" {
		t.Fatalf("legacy identity = %q, %v", stored, err)
	}
	for _, tc := range []struct {
		name  string
		value any
		valid bool
	}{
		{"subscription", "subscription", true}, {"api_credit", "api_credit", true},
		{"null", nil, true}, {"unknown", "other", false}, {"empty", "", false},
		{"number", float64(1), false}, {"boolean", true, false},
	} {
		for _, source := range []string{"worker", "intake"} {
			for _, provider := range []string{"claude", "codex"} {
				t.Run(tc.name+"/"+source+"/"+provider, func(t *testing.T) {
					row := map[string]any{"at": "2026-11-01T00:00:00.000Z", "host": "nas", "source": source, "provider": provider, "billing": tc.value}
					if got := ValidUsage(row); got != tc.valid {
						t.Fatalf("ValidUsage = %v, want %v", got, tc.valid)
					}
				})
			}
		}
	}
}

func TestBillingStoreAndRead(t *testing.T) {
	db := migratedUsageDB(t)
	ctx := context.Background()
	usageStore := Store{DB: db}
	rows := []map[string]any{
		{"at": "2026-11-01T00:00:00.000Z", "host": "nas", "source": "worker", "provider": "claude", "billing": "subscription"},
		{"at": "2026-11-01T00:00:01.000Z", "host": "nas", "source": "intake", "provider": "codex", "billing": "api_credit"},
		{"at": "2026-11-01T00:00:02.000Z", "host": "nas", "source": "worker", "billing": nil},
		{"at": "2026-11-01T00:00:03.000Z", "host": "nas", "source": "worker"},
		{"at": "2026-11-01T00:00:04.000Z", "host": "nas", "source": "worker", "billing": "other"},
		{"at": "2026-11-01T00:00:05.000Z", "host": "nas", "source": "worker", "billing": 1},
		{"at": "2026-11-01T00:00:06.000Z", "host": "nas", "source": "worker", "billing": ""},
	}
	counts, err := usageStore.AddRows(ctx, "test", rows)
	if err != nil || counts.Added != 4 || counts.Rejected != 3 || counts.Duplicate != 0 {
		t.Fatalf("AddRows = %+v, %v", counts, err)
	}
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got, skipped, err := usageStore.Read(ctx, "", from, from.Add(time.Hour))
	if err != nil || skipped != 0 || len(got) != 4 {
		t.Fatalf("Read = %+v, skipped %d, %v", got, skipped, err)
	}
	for i, want := range []any{"subscription", "api_credit", nil, nil} {
		value, exists := got[i]["billing"]
		if value != want || exists != (i < 3) {
			t.Errorf("row %d billing = %v (exists %v), want %v", i, value, exists, want)
		}
	}
	counts, err = usageStore.AddRows(ctx, "test", rows[3:4])
	if err != nil || counts.Duplicate != 1 || counts.Added != 0 {
		t.Fatalf("legacy duplicate = %+v, %v", counts, err)
	}
}

func TestDiagnosticKindValidationAndStore(t *testing.T) {
	kinds := []string{"completed", "timeout_idle", "timeout_total", "canceled_by_server", "spawn_failed", "output_overflow", "exit_nonzero", "authentication", "rate_limit", "usage_limit", "format", "session", "input_invalid", "execution"}
	base := func(source string, second int) map[string]any {
		return map[string]any{"at": fmt.Sprintf("2026-11-01T00:00:%02d.000Z", second), "source": source, "host": "nas", "inputTokens": nil, "billing": "subscription", "firstOutputMs": float64(0)}
	}
	valid := []map[string]any{base("intake", 0)}
	for i, kind := range kinds {
		row := base("intake", i+1)
		row["diagnosticKind"] = kind
		valid = append(valid, row)
	}
	for _, row := range valid {
		if !ValidUsage(row) {
			t.Fatalf("valid row rejected: %#v", row)
		}
	}
	invalid := []map[string]any{}
	for i, value := range []any{nil, "", "unknown", float64(1), true, []any{}, map[string]any{}} {
		row := base("intake", i+20)
		row["diagnosticKind"] = value
		invalid = append(invalid, row)
	}
	for i, value := range append([]any{nil, "", "unknown", float64(1), true}, stringValues(kinds)...) {
		row := base("worker", i+30)
		delete(row, "firstOutputMs")
		row["diagnosticKind"] = value
		invalid = append(invalid, row)
	}
	for _, row := range invalid {
		if ValidUsage(row) {
			t.Fatalf("invalid row accepted: %#v", row)
		}
	}

	db := migratedUsageDB(t)
	ctx := context.Background()
	s := Store{DB: db}
	counts, err := s.AddRows(ctx, "test", append(valid, invalid...))
	if err != nil || counts.Added != len(valid) || counts.Rejected != len(invalid) || counts.Duplicate != 0 {
		t.Fatalf("AddRows = %+v, %v", counts, err)
	}
	counts, err = s.AddRows(ctx, "test", valid[:1])
	if err != nil || counts.Duplicate != 1 || counts.Added != 0 {
		t.Fatalf("legacy duplicate = %+v, %v", counts, err)
	}
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got, skipped, err := s.Read(ctx, "", from, from.Add(time.Hour))
	if err != nil || skipped != 0 || len(got) != len(valid) {
		t.Fatalf("Read = %#v, skipped %d, %v", got, skipped, err)
	}
	for i, row := range got {
		value, exists := row["diagnosticKind"]
		if exists != (i > 0) || (i > 0 && value != kinds[i-1]) {
			t.Errorf("row %d diagnosticKind = %v, present %v", i, value, exists)
		}
		if v, ok := row["inputTokens"]; !ok || v != nil || row["billing"] != "subscription" || row["firstOutputMs"] != float64(0) {
			t.Errorf("row %d existing fields changed: %#v", i, row)
		}
		if _, ok := row["outputTokens"]; ok {
			t.Errorf("row %d absent measurement added: %#v", i, row)
		}
	}
}

func stringValues(values []string) []any {
	result := make([]any, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func TestIntakeCallIdentityPreservesDistinctRows(t *testing.T) {
	db := migratedUsageDB(t)
	store := &Store{DB: db}
	row := map[string]any{"at": "2026-11-01T00:00:00.000Z", "host": "nas", "source": "intake", "provider": "codex", "model": "fixture", "step": "draft", "callId": "11111111-1111-4111-8111-111111111111"}
	add := func(r map[string]any) Counts {
		t.Helper()
		counts, err := store.AddRows(t.Context(), "intake", []map[string]any{r})
		if err != nil {
			t.Fatal(err)
		}
		return counts
	}
	if c := add(row); c.Added != 1 {
		t.Fatal(c)
	}
	if c := add(row); c.Duplicate != 1 {
		t.Fatal(c)
	}
	changed := map[string]any{}
	for k, v := range row {
		changed[k] = v
	}
	changed["outputTokens"] = float64(7)
	if c := add(changed); c.Rejected != 1 || c.Added != 0 {
		t.Fatal(c)
	}
	other := map[string]any{}
	for k, v := range row {
		other[k] = v
	}
	other["callId"] = "22222222-2222-4222-8222-222222222222"
	if c := add(other); c.Added != 1 {
		t.Fatal(c)
	}
	rows, skipped, err := store.Read(t.Context(), "", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || skipped != 0 || len(rows) != 2 {
		t.Fatalf("rows=%v skipped=%d err=%v", rows, skipped, err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[fmt.Sprint(r["callId"])] = true
		if _, ok := r["outputTokens"]; ok {
			t.Fatal("unknown usage was replaced", r)
		}
	}
	if !seen[row["callId"].(string)] || !seen[other["callId"].(string)] {
		t.Fatal(rows)
	}
}
