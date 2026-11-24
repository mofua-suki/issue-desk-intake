package statistics

import (
	"os"
	"testing"

	"issue-desk-intake/server/internal/appsql"
)

// The application store is PostgreSQL only; the package-level dialect follows.
func TestMain(m *testing.M) {
	appsql.SetActive(appsql.Postgres)
	os.Exit(m.Run())
}
