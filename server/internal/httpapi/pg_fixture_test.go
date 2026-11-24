package httpapi

import (
	"os"
	"testing"

	"issue-desk-intake/server/internal/appsql"
)

// このパッケージの試験は本番と同じPostgreSQL方言で動かす。
func TestMain(m *testing.M) {
	appsql.SetActive(appsql.Postgres)
	os.Exit(m.Run())
}
