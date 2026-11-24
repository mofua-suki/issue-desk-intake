# 起票比較資料の祖先読取と専用計測

`readMaterialDrafts` は、判定の `decisionContext` と同じく対象自身を除き、祖先を最大64件参照する。欠損した親では終了し、資料には `unavailable` を残す。循環でも終了する。上限外の祖先でも、独立した比較候補として必要なら全文を取得する。既存の未起票兄弟の選択も独立に保持する。

本番の `consultations.Records.ReadFilingSnapshot` を通し、読取専用REPEATABLE READ内の投影とDBキーによる選択全文を組み合わせる。SQLState 22P02・22P05の投影失敗時だけ同じトランザクションで全件読取に戻り、その他の失敗は伝播する。

## 専用計測

リポジトリルートで、READMEと同じGo・Node・PostgreSQL環境を使う。

```sh
ISSUE_DESK_MEASURE_MATERIAL=1 node scripts/with-test-postgres.ts -- sh -c 'cd server && go test -timeout=0 -count=1 -v ./internal/filingcheck -run "^TestMeasureMaterialDrafts$"'
```

専用計測には `ISSUE_DESK_MEASURE_MATERIAL=1` が必要。通常検証のSKIPは計測の成功を意味しない。専用計測は時間制限を設けない。既存の一覧API計測は代用しない。

実DBの本番スキーマ・接続層・本番の投影/選択SQLを使い、ドライバーの `Rows.Next` が実際に返す値を集計する。バイト数はstring/[]byteのUTF-8長の和（DBキーを含む）。SQLの引数、プロトコル、ディスク/TOAST容量、復号後のオブジェクト容量は含まない。失敗前に返った行は返った分だけ数える。投影・選択全文・互換全件読取を別々に記録する。

読取総時間は関数呼出から戻りまでで、接続の取得待ちを含む。保持時間はドライバーのBeginTx成功直後からCommit/Rollbackの完了まで。計測用の専用接続を事前にPingし、MaxOpenConns=1・逐次実行とする。準備・挿入・例外行の変更は計測外。パス/候補割合ごとに両方式を1回ずつウォームアップし、続く3反復は先行方式を交替する。OS/DBキャッシュを消去しないためウォーム状態の比較であり、コールド読取ではない。

比較の相手は `material_baseline_test.go` が再現した祖先無制限の旧方式（計測名 `65c7199-reconstructed`）。合成データは実データの分布の推定ではないので、合成の結果を本番相当DBでの計測の代わりにしない。
