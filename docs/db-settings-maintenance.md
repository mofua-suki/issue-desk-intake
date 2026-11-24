# 共有DBの設定保守

通常のServer更新はDBの設定を反映しない。共有DBの停止は両色に影響し、Serverの交代・切り戻しはDB設定を扱わないため、別の保守時間に実施する。受付停止開始からDB再作成・健康確認・受付再開まで停止する。DBの停止猶予は60秒で、復旧処理や起動待ちも加わるため、実測のない停止秒数は保証できない。

対象版、停止対象、停止見込み、復旧手順について個別の承認を得て、更新や他の保守を排他する。以下はリポジトリのルートから、配置時と同じComposeプロジェクト名・環境で実行する。DB以外の書き手や受付への入力も停止し、処理の完了を確認する。各確認やコマンドが失敗した場合は次へ進まず、受付を停止したまま原因を調査する。

1. 現在のComposeファイルを配置変更前に安全な場所へ保存し、旧設定への復旧に使う。旧ファイルが現在のDBの起動設定と一致しない、または同じプロジェクト・ボリュームを指すことを確認できない場合は中止する。稼働中の設定を記録し、既定値と決めつけない。最新バックアップの成功ログと生成された非空ファイルを確認する（DB停止中のバックアップは成功ではなくスキップされる）。DBボリュームの実体があるファイルシステムの空きを確認し、既存使用量と運用上の余裕を含め十分か判断する。`max_wal_size` は厳密な容量上限ではない。確認できない場合は停止しない。

   ```sh
   cp deploy/nas/compose.yaml /tmp/issue-desk-compose-before-db.yaml
   sh deploy/nas/intake.sh probe
   sh deploy/nas/intake.sh status
   docker exec issue-desk-db psql -U issue_desk -d issue_desk -c "SELECT name, setting, unit, source FROM pg_settings WHERE name IN ('wal_compression','max_wal_size','checkpoint_timeout','synchronous_commit','fsync','full_page_writes') ORDER BY name;"
   sh deploy/nas/db-backup.sh
   docker volume inspect issue-desk-db --format '{{.Mountpoint}}'
   df -h "$(docker volume inspect issue-desk-db --format '{{.Mountpoint}}')"
   ```

   バックアップコマンドの `[db-backup:success]` と表示されたファイルを `test -s <表示されたパス>` で確認する。`probe` の一意のコンテナと `status` の稼働色を記録し、入口の転送先がその色であることも確認する。不一致や判定不能なら中止する。新しいComposeを配置後も、保存した旧ファイルと記録した値を保持する。
2. 受付を停止し、両色が停止したことを確認する。`down` や `--volumes` は使わない。

   ```sh
   docker compose --project-directory deploy/nas -f deploy/nas/compose.yaml --profile intake stop issue-desk-intake-blue issue-desk-intake-green
   docker compose --project-directory deploy/nas -f deploy/nas/compose.yaml --profile intake ps --status running issue-desk-intake-blue issue-desk-intake-green
   ```

   出力に稼働中の色が残る場合や確認が失敗した場合はDBを操作しない。
3. 新しいComposeを配置し、既存ボリュームを保持してDBだけを再作成する。健康状態が `healthy` になるまで待ち、失敗・停止・健康にならない場合は受付を再開せず復旧する。設定と耐久性設定を手順1の記録と照合する。

   ```sh
   docker compose --project-directory deploy/nas -f deploy/nas/compose.yaml --profile intake up -d --no-deps --force-recreate issue-desk-db
   docker inspect --format '{{.State.Status}} {{.State.Health.Status}}' issue-desk-db
   docker exec issue-desk-db psql -U issue_desk -d issue_desk -c "SELECT name, setting, unit, source FROM pg_settings WHERE name IN ('wal_compression','max_wal_size','checkpoint_timeout','synchronous_commit','fsync','full_page_writes') ORDER BY name;"
   ```

   `wal_compression` が `pglz`（`on` の指定に対応）、`max_wal_size` が4GB、`checkpoint_timeout` が15分に相当し、耐久性3設定が旧値と同じことを確認する。値や健康状態が違えば再開しない。
4. 記録した元の稼働色だけを起動する（例は blue。green ならサービス名を置き換える）。`probe` が一意の元の色を返し、`status` が正常であること、入口経由の応答が正常で同じ色が書き手であることを確認する。失敗時は受付を停止して調査する。

   ```sh
   docker compose --project-directory deploy/nas -f deploy/nas/compose.yaml --profile intake up -d --no-deps issue-desk-intake-blue
   sh deploy/nas/intake.sh probe
   sh deploy/nas/intake.sh status
   docker compose --project-directory deploy/nas -f deploy/nas/compose.yaml --profile intake exec -T issue-desk-intake-entry wget -q -O - http://127.0.0.1:8787/healthz
   ```

設定を戻す場合は両色を停止・停止確認した後、保存した旧Composeで同じプロジェクトとボリュームを使い、DBを再作成する。旧ファイルの場所を変えても `--project-directory deploy/nas` を維持する。

```sh
docker compose --project-directory deploy/nas -f /tmp/issue-desk-compose-before-db.yaml --profile intake up -d --no-deps --force-recreate issue-desk-db
docker inspect --format '{{.State.Status}} {{.State.Health.Status}}' issue-desk-db
docker exec issue-desk-db psql -U issue_desk -d issue_desk -c "SELECT name, setting, unit, source FROM pg_settings WHERE name IN ('wal_compression','max_wal_size','checkpoint_timeout','synchronous_commit','fsync','full_page_writes') ORDER BY name;"
```

健康状態と記録した旧値を確認してから手順4で元の色だけを再開する。失敗したら停止したまま調査する。通常の設定切り戻しではバックアップを復元しない。データ復元が必要な場合は別途判断・承認し、[DB復旧手順](pg-cutover.md)に従う。
