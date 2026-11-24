# GET /api/list の差分応答

`GET /api/list` は `since` のカーソルで前回の応答との差分を返す。`tabs=active` は要対応・進行中の表示に必要な行を取得し、`tabs` を省略すると全体を取得する。応答の `scope` はそれぞれ `active`・`full` になる。差分エンジンは `server/internal/screenapi/delta.go`（比較材料の生成・共通化は `delta_material.go`）で、`startup/screen.go` が読取元を登録し、`screenapi/service.go` がCookie・Origin・許可プロジェクト・取得範囲からscopeを作って `Respond` を呼ぶ。カーソルはセッション・Origin・許可範囲・取得範囲ごとに別で、ほかのscopeのbaselineを使わない。

行の `item` は一覧が読む項目だけを返す（`screenapi/list_row.go` の `Summarize`）。Issueの行は `GET /api/issues` の行から、画面の一覧が読まない `dispatchRecord`・`executionSettings`・`dispatchSkip`・`workType`・`publishPending`・`releasePending`・`executionWait` を除く。`GET /api/list/entry` の行も同じで、除いた項目だけの変化は差分にならない。これらは `GET /api/issues`・`GET /api/issue` で読む。

baselineはプロセス内だけに置き、保持は64件・5分・合計16 MiB（16,777,216 bytes）まで。超えた分は古い順に退去する。単一のbaselineが上限を超えるときは保持せず、`cursor=null` の全体応答を返す（次回は `since` なしで取得する）。永続データには残さない。

比較材料のJSONは行keyの種別ごとに共通の接頭辞・接尾辞を一度だけ保持し、中間部を独立した文字列にコピーする（元のJSON全体を参照で残さない）。比較は文字列の完全一致で、ハッシュの衝突を許さない。論理容量は共通化前のUTF-8 JSON長とkey長で数え、欠測を引き継ぐ場合も元の長さを足す。短い保持表現を理由に16 MiBの上限を緩めない。

合成データでの速度・割当の比較は `server/internal/screenapi` の `BenchmarkDeltaComparison`（`delta_reference_test.go` の参照処理と比べる）で測る。速度は通常テストの合否に使わない。

## jobの変更通知

`jobs.Store` は一覧に関係するjobの列、依存の辺、配布履歴を変更したトランザクションのコミット後に、`type=issue`・`project=repo`・`number=issue` を通知する。同じトランザクション内の複数の変更はissueごとにまとめる。Clientは既存の `GET /api/list/entry` で対象行を読み直す。leaseの期限・進捗時刻・`view.progress` だけの更新では通知しない。列と経路の対応は `server/internal/jobs/changes.go` とPostgreSQLのマイグレーション34に置く。

変更対象の収集はトランザクション内だけに保持する。通知は永続キューではなく、コミット直後のプロセス停止や接続切断では失われ得るため、定期取得と復帰時の照合を維持する。`Store` の `Publish` は省略可能。Storeの共通トランザクションを使わない書き手は、必要な通知を自分のコミット後に送る。

マイグレーション34は収集用の関数とトリガーを追加し、jobの保存形式や既存データを変更しない。収集を有効にしない旧Serverではトリガーは何も記録しない。Serverを戻す場合も適用済みマイグレーションとトリガーを保持し、保存済みjobをそのまま読み込む。再更新は通常の起動時マイグレーションで行う。

## 一覧の所要の読み方

`GET /api/list` の `Server-Timing` と `GET /api/meta` の `listMeasurements` は、`issue_read`・`draft_read`・`attentionAlerts_read` などの読取開始から終了までの経過時間を記録する。読取は並行して動き、要求内で共有する読取の待ちも含むため、各段の値を合算して総所要やDB処理時間として扱わない。

`tabs=active` の `draft_read` は、相談の候補読取・家族進捗に加え、残す相談を決めるためのIssue読取の完了待ちを含む。`issue_read` も相談の候補読取と開いたクローズ依頼の読取を共有し、その完了待ちを含む。共有読取は最初の呼出し元で実行するため、両方が長い場合に相談とIssueそれぞれが同じ長さの処理をしたとは判断できない。`attentionAlerts_read` の内訳は `attentionAlerts_drafts`・`attentionAlerts_saved`・`attentionAlerts_notices`・`attentionAlerts_stage`・`attentionAlerts_incidents`・`attentionAlerts_jobs`・`attentionAlerts_jobRecovery`・`attentionAlerts_checkout`・`attentionAlerts_rework`・`attentionAlerts_rc`・`attentionAlerts_visible` を参照する。

比較は同じデータ・取得範囲で複数回行い、冷えた初回と続く読取を区別して、行数・応答量・総所要と各段の値を記録する。長い読取と共有依存の関係、Attentionの内訳から支配的な経路を絞り込む。ブラウザの `firstDisplayMs` は通信・描画も含むため、Serverの総所要だけでは表示目標の達成を判定しない。
