package startup

import (
	"context"
	"strings"
	"testing"
)

func TestRecordServerRevisionAppendsTakeoversPostgres(t *testing.T) {
	db := appPostgres(t)
	ctx := context.Background()
	revision := strings.Repeat("a", 40)
	// 再起動・切り戻しでも取った順を残す（どれが配備かは集計が決める）。開発版の名前は記録しない。
	recordServerRevision(ctx, db, revision, nil)
	recordServerRevision(ctx, db, revision, nil)
	recordServerRevision(ctx, db, "development", nil)
	var count, distinct int
	if err := db.QueryRowContext(ctx, "SELECT count(*),count(DISTINCT revision) FROM server_takeovers WHERE revision=$1", revision).Scan(&count, &distinct); err != nil {
		t.Fatal(err)
	}
	var all int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM server_takeovers").Scan(&all); err != nil {
		t.Fatal(err)
	}
	if count != 2 || distinct != 1 || all != 2 {
		t.Fatalf("count=%d distinct=%d all=%d", count, distinct, all)
	}
	// 記録に失敗しても起動を止めず、理由を残す。
	if _, err := db.ExecContext(ctx, "DROP TABLE server_takeovers"); err != nil {
		t.Fatal(err)
	}
	var logged []string
	recordServerRevision(ctx, db, strings.Repeat("b", 40), func(message string) { logged = append(logged, message) })
	if len(logged) != 1 || !strings.Contains(logged[0], "受付の版の記録に失敗しました") {
		t.Fatal(logged)
	}
}
