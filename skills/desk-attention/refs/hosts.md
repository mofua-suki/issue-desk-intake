# PCの診断情報を読む

汎用窓口の接続・`body`・`redacted`・終了コードは [共通API手順](../../issue-desk-api/SKILL.md) の「汎用窓口の応答」に従う。

`GET /api/hosts` は通常のPCトークンで読める。利用者がPCの状態を尋ねた場合や、許可済み作業に必要なときに読む。NASでは次の汎用窓口、Ubuntuでは `~/.issue-desk/desk get` を使う。

```sh
sh ~/.issue-desk/desk-pc get --path /api/hosts
```

HTTP成功と応答 `body.hosts` が配列であることを確認する。`body.reason`、`source`、`checkedAt`、`leaseMinutes`、`homeMonitor` と取得時刻を残す。理由のない空配列は取得できた0件、理由付き空一覧は取得不能として区別する。401・403、通信失敗、不正な応答は未確認とし、例外本文や接続設定に含まれ得る資格情報を表示しない。

各PCはAPIが算出した値を読み、欠測をfalseや0に変えない。

- 版と配布: `revision`・`imageRevision`、`rollout.stage`・`role`・`reason`、`image.desired`、`failedTags`、日時降順の `results`、`lastFailure`。`imageUnavailable` は未確認の理由。自己検査の不合格は `selfCheck.failed`。
- 引き取り: `ready`・`busy`・`implementing`・`publishing`、`intakeHold.reasons` のkind/since/detail。usage_limitはprovider/until/limitKindも読む。版ずれは `driftHold` のkind/since/current/desired。これらを担当リポジトリの割当と混同しない。
- 停止とheartbeat: `drain` のreason/at/by/until、`updatedAt`、`alive`、`restartSafe`。欠測・古いheartbeatは正常や再起動可にしない。
- 負荷: `overloaded` と `load` の現在値、`loadHistory` の直近3時間のmax/average/count。対象はcpuPercent/freeMemoryMB/freeDiskMB/loadAverage1m/logicalCpuCount/gpuPercent/usedVramMB/freeVramMB。数値0は有効、null・欠落は欠測。標本数と期間を揃えず最大/平均だけ比較しない。
- hostd周期: `hostdHealth.cycle` のresult/stage/exceptionType/at/receivedAt/failures/lastSuccessAt。更新結果とheartbeatを取得時刻に照合する。

取得だけでは停止・再開・再試行しない。操作は各Skillと利用者の依頼範囲に従う。
