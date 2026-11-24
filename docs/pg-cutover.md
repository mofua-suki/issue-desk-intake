# PostgreSQLの切替の記録

受付の保存先は PostgreSQL の `issue_desk_app_migration` スキーマだけ。スキーマの定義は `server/internal/appdb`（`CreateSchema`）で、テスト用 PostgreSQL の `internal/pgtest` も同じ定義を使う。SQLite のドライバ・方言・移行コマンドは無い。

起動時に `app_store_cutover` テーブル（`server/internal/appcutover`、`appcutover.Done`）の記録を確かめ、無ければ起動を止める。空の保存先で始めて既存の相談・認証を失わないため。設定に SQLite のファイルが残っていれば、その名前も示して止める（ファイルには触れない）。

止まったときは、接続先（`ISSUE_DESK_DATABASE_URL` または `PGHOST`・`PGUSER`・`PGDATABASE`）が本番の共有DBを指しているかを確かめる。共有DBを失ったときは `deploy/nas/db-backup.sh` が残す `runtime/db-backups/` の `pg_restore` 形式の控えから戻す。切替の記録を手で作って空の保存先で起動しない。

`runtime/state` の番号台帳（`intake-desk-numbers.jsonl`）は今も使うので消さない。

関係図用の射影列と保存トリガーは受付の起動時に作成し、欠けている射影を既存の相談・TODOから再生成する。古いバックアップを復元した場合も起動時に再生成される。生成できない本文は元のJSONから読み取る。既存行の再生成はDDL完了後に最大50行ずつ確定し、中断時は次回起動で未生成行から再開する。大量の既存行がある場合は初回起動時に処理時間とDBの書込負荷が増える。

相談画面用の射影 `drafts.screen_summary` も受付起動時に列・関数・トリガーを導入し、未生成または旧版の行を最大50行ずつ確定する。射影の形式を変更するときはトリガーと補完処理の形式番号を共に更新し、起動時の補完で旧版を再生成する。古いバックアップを復元した場合や途中で起動が中断された場合は次の起動で補完する。生成できない不正なJSONや対応できないUnicodeの行はNULLのまま残る。大量の未生成行がある場合、補完が終わるまで受付の起動が遅れ、DBの書込負荷が増える。
