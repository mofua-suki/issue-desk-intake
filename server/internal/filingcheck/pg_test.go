package filingcheck

import (
	"os"
	"testing"

	"issue-desk-intake/server/internal/appsql"
)

// The production process runs on PostgreSQL (appsql.SetActive at startup), so
// the tests expand SQL for PostgreSQL as well.
func TestMain(m *testing.M) {
	appsql.SetActive(appsql.Postgres)
	os.Exit(m.Run())
}
