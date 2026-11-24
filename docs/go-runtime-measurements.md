# Go Serverの稼働計測

`GET /api/meta` の `runtimeMeasurements` は `source: go-runtime` の実測で、Serverの起動後から最後の観測までを `from`・`to` に示す。起動直後の未観測値はnullと理由を返し、0で埋めない。サンプルは再起動でリセットする。

1秒の周期ごとに、Goタイマーの予定時刻と実際にgoroutineが動いた時刻の差を測る。`timerDelayMsMean`・`timerDelayMsMax` はその平均と最大、`samples` は観測回数。周期の処理時間は次の予定時刻へ足し込まず、DB応答の遅れをタイマー遅延として重複計上しない。

同じ周期で共有DBへ `SELECT clock_timestamp()` を送り、単調時計で往復時間を測る。`dbRoundTripMsMean`・`dbRoundTripMsMax` は成功した計測の平均と最大。`dbSamples` と `dbFailures` を分け、失敗を0msの成功に含めない。`dbClockOffsetMsLower`・`dbClockOffsetMsUpper` は最後の成功でDB時計を受け取った前後のホスト壁時計から求める差の下限と上限。ネットワークが対称、または時計が同期しているとは仮定しない。

この値はNode.jsのevent loop利用率・CPU profileとは別の観測で、旧値へ換算・混合しない。保存済みのNode.js CPU profileは既存の `GET /api/cpu-profiles` と詳細APIで読める。

受付AIの `measurement` 参照資料はNASの工程開始・終了・中断記録と、共有DBに集まった全PCの使用量を別の観測範囲として表示する。工程は8000bytes、使用量要約は3000bytes、詳細は2000bytesに収め、欠測と省略を明示する。`rework-by-size` は今日を含む日本時間30日のマージ済み依頼を母集団とし、選択済みプロジェクトと全体、見込み対実際、少数件数の注記を6000bytes以内に示す。

`SystemAlerts` はGoタイマー遅延が2000msを1分ごとの観測で3回続けて超えると要対応を出し、10分の連続した正常観測で解除する。丸ごと観測できなかった分を正常窓や超過窓として補完しない。IDと種別は `intake-go-timer-delay` / `intake_runtime_delay` であり、旧Node.jsの警告と区別する。DB往復の長さを、Node.jsの同期処理時間として読み替えることはしない。

## 受付画面の一覧の計測

`GET /api/meta` の `listMeasurements` で、受付画面の一覧 `GET /api/list` の直近の実測を読める。一覧と計測値は画面のセッションとローカルCLI一時キー（`~/.issue-desk/desk desk-get.ts GET /api/meta`）で読める。委任PCトークンは一覧の件数取得（`GET /api/list?view=counts`）に使える。行の内容・識別子は持たず、数値だけを持つ。Serverのメモリに種類ごとに最新20件を置き、再起動（受付の更新）で消える。

- `full`・`delta`: 全体の応答と `since` の差分の応答。`bytes` はServerが作った圧縮前の大きさ、`rows` は行数、`totalMs` と `stages`（読取の段ごと・差分の計算・JSON化）はServer内の所要。同じ段の値は応答ヘッダ `Server-Timing` にも返す。全体の応答が2000ms以上かかったときは段ごとの所要をログへ書く。
- `browser`: 画面が保持の無い状態で一覧を開いたときに `POST /api/list/timing` で報告する値。`fetchMs` は取得、`ttfbMs` は最初の1バイトまで、`integrateMs` は取り込み、`drawMs` は描画、`firstDisplayMs` は取得の開始から描画の終わりまで、`paintMs` は次のフレームまで（裏のタブでは無い）、`fromNavigationMs` はページを開いてから描画の終わりまで（ページの読み込み直後の表示だけ）。`encodedBytes` は圧縮後、`decodedBytes` は圧縮前の大きさ。同じ値を `一覧の表示を計測しました` の行でログへも書く。

DBキーとJSON内IDの不一致などで画面一覧が従来の家族別全文読取へ戻ると `draft screen snapshot identifiers invalid; using full family reads` がログに出る。不正な行の総数 `invalid rows` と読取順の先頭最大5件のDBキー `first DB keys` を含み、同じサービスでは最大1分に1回出力する。

## PC負荷標本の保持

`host_load_samples` は各PCのハートビートで追加する。期限切れ行の整理は、初回と前回の整理から10分以上経過した同じPCのハートビートで行い、受領から3時間より古い標本を対象にする。通知が止まったPC・廃止したPCの標本は、そのPCが再び通知するまで保存される。全PCを対象とする定期削除は行わない。通知のないPCの保存行が残っていても、画面用の時系列は現在から過去3時間の標本だけを読み取る。
