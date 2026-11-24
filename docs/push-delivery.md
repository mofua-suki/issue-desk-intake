# Web Pushの送信と検証

`POST /api/push/test`と公開関数`adminapi.NotifyPhone`は同じ送信処理を使う。
通知1回につき保存済み購読のスナップショットを読み、最大4件を同時に送信する。
状態ファイルのロックはHTTP通信中には保持しない。結果集計、失敗ログ、失効購読の削除は呼出元で順次処理する。
404/410が正常に受信できた場合だけ、最新の保存状態をロック内で読み直し、endpoint・auth・p256dhがすべて一致する購読を削除する。
送信中の鍵更新や購読追加は保持する。HTTP失敗、timeout、キャンセルでは削除しない。

呼出しcontextのキャンセル後は未開始分を失敗として数え、開始済みの送信と応答本文の後始末を回収してから戻る。
送信ごとのcontextとHTTPクライアントのtimeoutは最大10秒で、注入された短いクライアントtimeoutも維持する。
リダイレクトは追跡しない。送信先検証、暗号化・署名、状態ファイルの0600権限、認証・認可・Origin・CSRF条件はほかのAPIと同じ。
テスト通知は固定本文を全購読へ送り、送信失敗でもHTTP 200と`ok:true`を返す。

AI失敗通知・ドリフト通知（`server/internal/startup/observability.go`）と運用監視通知（`server/internal/startup/operations.go`）にも同じ並行化が適用される。
利用者判断待ち通知は、保留中のIssueの権限・削除・最終承認・実装回数上限・使用量上限の待ちを1分ごとに収集し、新たな対象を1通にまとめる。通知先は要対応画面 `/#/list/attention`。プロジェクト・Issue番号・待ちの種類ごとに通知前に予約を保存し、同じ待ちが続く間は再送しない。送信失敗、購読なし、予約後の停止でも再送せず、配達は保証しない。正常な収集で消失が記録された後に再出現した待ちは再通知する。通知は承認の代わりにはならない。

上限4件は呼出し単位であり、同時に複数回呼び出す場合の合計を制限しない。
呼出元とブラウザのテスト通知ボタンは全送信の完了を待つ。遅い通知先自身のtimeout待ち、状態ロック・保存処理の待ちは残る。

## 検証

READMEのGo/Node環境で実行する。すべて合成購読とfake Transportを使い、実際の通知サービスや本番データには接続しない。

```sh
node scripts/with-test-postgres.ts -- sh -c 'cd server && go test ./internal/adminapi ./internal/auth -count=1 -v && go vet ./internal/adminapi ./internal/auth'
cd server
go test ./internal/adminapi -run '^TestPushPerformanceComparison$' -count=1 -v
# Cコンパイラとcgoが利用可能な環境で実施する。
CGO_ENABLED=1 go test -race ./internal/adminapi -run '^TestPush(Bounded|Cancellation|Mixed|Invalid|Delivery|Concurrent|Expired|Performance)' -count=1
```

`TestPushPerformanceComparison` は `push_sequential_test.go` に残した以前の順次送信と並行送信を同じ合成購読で比べ、待ち時間を出力する。経過時間に合格閾値は設けず、別の回帰試験で先頭の遅い通知先をチャネルで止めたまま、5件目以降の成功が完了することを検証する。

全体の固定検証はREADMEの`sh scripts/verify.sh`を使う。DBを使う試験のSKIPは成功に数えない。
