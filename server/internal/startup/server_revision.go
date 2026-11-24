package startup

import (
	"context"
	"database/sql"
	"regexp"
)

var serverRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// recordServerRevision appends each time a revision takes the leader lease,
// including restarts and switch-backs; the overview decides from the order which
// takeover deployed a revision. A failure only loses that measurement, so
// startup continues.
func recordServerRevision(ctx context.Context, db *sql.DB, revision string, log func(string)) {
	if db == nil || !serverRevisionPattern.MatchString(revision) {
		return
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO server_takeovers(revision)VALUES($1)", revision); err != nil && log != nil {
		log("受付の版の記録に失敗しました: " + err.Error())
	}
}
