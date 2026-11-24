# 受付AIの呼び出しの診断

受付AI（相談・初期判定・起票確認）の呼び出しは、成功も失敗も、1回ごとに診断（`ai.Answer.Diagnostic`）を残す。使用量を確定できない呼び出しの後に自動の呼び出しを止める「使用量の保留」（`usageMeasurementHold`）は、その診断と一緒に保存する。止まった原因が、使用量の不明に覆われて見えなくなることを防ぐ。

## 診断に入るもの

固定の識別子と数値だけ。プロンプト・回答・stderr・環境変数・資格情報・URLは入れない。

- `id`: 呼び出しごとの診断ID（保留・停止の文にも出る）
- `kind`: `completed`・`timeout_idle`（無出力が続いた）・`timeout_total`（全体の制限時間）・`canceled_by_server`（サーバーの停止・再起動などで中断）・`spawn_failed`・`output_overflow`・`exit_nonzero`・`authentication`・`rate_limit`・`usage_limit`・`format`・`session`・`input_invalid`・`execution`
- `stage`: `preflight`（実行前の検査）・`spawn`・`run`・`exit`（プロセス終了後）・`answer`（回答の検査）・`done`
- `elapsedMs`・`firstOutputMs`・`lastOutputMs`（最初・最後の出力までの時間。出力が無いときは null）・`stdoutBytes`・`stderrBytes`
- `exitCode`: 終了コードが意味を持つときだけ（時間切れ・中断で強制終了したときは null）
- `finalEvent`: 出力の最後のJSON行の `type`（既知のイベント識別子だけ。不明な値は null）
- `failureReason`: `exit_nonzero` の固定分類。最後の `turn.failed` のエラー、失敗したClaudeの `result` イベントの本文と標準エラーから `authentication`・`input_invalid`・`rate_limit`・`usage_limit`・`provider_unavailable`（提供元のサーバーエラー・過負荷・接続切断）を識別し、それ以外は `unknown`。5xx の数値は status・error・HTTP などの状態を示す語に隣接するときだけ提供元のエラーとして扱う。元の文章は保存しない。
- `usageEvidence`: 相談の下書き・起票前確認の実行開始失敗を直接確認した `process_not_started`、またはその工程の失敗時に必要なトークン値を完全に実測して全て0だった `measured_zero`。根拠がなければ省略する。前者の0は実測値ではない。
- `idleCondition`: `timeout_idle` で実際に発動した条件。`output_deadline` は標準出力・標準エラーの出力期限、`inactivity_deadline` は出力とプロセス活動の無活動期限を示す。旧記録や条件を観測できない記録、他の終了理由では省略する。
- `idleProcess`: `timeout_idle` のときだけ付く、強制終了の直前のプロセスグループの様子（下の「無出力の強制終了の直前の記録」）

不明な値は 0 にせず null にする。

相談の下書きと起票前確認では、実行器が当該AIプロセスの開始失敗を直接確認したときだけ、必要なトークン使用量を未実行の0として返す。Codexが提供しないキャッシュ書き込み量と不明な料金は空のまま。認証確認など本実行前の別プロセスの失敗、無出力、短時間、イベント不在や原因分類だけでは未実行と判定しない。部分計測や切詰めを含む未確定の使用量は保留して停止する。

後続の再試行実装は、直接確認した未実行か、必要な各使用量が欠測・未完了なく実測0の失敗に限り、監査と予算の保存成功および既存の上限検査を通した後に1回だけ追加実行できる。取消し・時間切れ・切詰め・保存失敗や未確定の場合はこの例外を使わない。この資料の診断生成だけでは再試行しない。

## 使用量の保留

使用量を確定できない呼び出し（時間切れ・中断で使用量が取れないとき、一部の欠測、記録の失敗）の後は、従来どおり自動の呼び出しを止める。0などで補って進めない。保留には `diagnostic`（上の診断）と `originalFailure`（元の失敗の種類）、`originalCause`（呼び出しの失敗の原因）、`recordCause`（使用量の保存の失敗の原因）を併記し、停止の文に「元の呼び出し: 無出力が続いて時間切れ（段階 exit・240秒・診断ID …）」のように出す。

Codexの再開時はrolloutを読み、累積使用量の差分を求める。終端改行のない最終行も完結したJSONなら読み取り、不正または不完全なJSONは使用量を欠測として保留する。1行が64 MiBを超える場合はサーバーのメモリを守るため読取を打ち切り、使用量を欠測として保留する。画像は1枚2 MiB、相談全体で12枚までで、画像を含む相談は既存セッションを再開せず新しく呼び出す。

完了の記録が残っていない呼び出し（`pendingCall`。サーバーの再起動などで途中で中断した可能性）と、使用量が未確定の呼び出しの件数も、停止の理由として示す。診断も原因も持たない古い保留は、従来と同じ文で止まる。

### 保留の解除

時間切れ・中断（診断の種類が `timeout_idle`・`timeout_total`・`canceled_by_server`）で使用量を取れなかった保留だけは、利用者が相談へ新しい返信を送ると解除される。解除しても使用量を0などで補わない。未確定だった呼び出しの印（`unknownCalls`・`pendingCall`）だけを外し、呼び出し回数と使用量の上限の数え方は変えない。解除は `consultationBudget.measurementReleases` に、時刻・発言ID・診断の種類・診断IDを残す。

- 保留が付かず、完了の記録が無い呼び出し（`pendingCall`。受付の再起動などで途中で中断した呼び出し）だけが残っている相談も、利用者の新しい返信で解除する（記録の `kind` は `interrupted`）。使用量が未確定の呼び出しの件数（`unknownCalls`）だけがある場合は、下書きの診断の記録（`aiDiagnostics`）の最後が `timeout_idle`・`timeout_total` のときだけ解除する（時間切れの後の1回だけの再試行も時間切れで終わった相談。記録の `kind` はその診断の種類）。それ以外は解除せず、停止の文にも返信での再開を出さない。
- 自動の再試行（stall の再試行など `retry` を付けた送信）では解除しない。解除するのは利用者が書いた返信だけ。停止の文にも、返信で再開できること（1相談3回まで）と再試行では再開しないことを出す。
- 認証・形式・使用量の保存の失敗など、時間切れ・中断以外の保留は解除しない。
- 1つの相談で解除できるのは3回まで（`maxMeasurementReleases`）。超えたら従来どおり止まる。

テスト: `server/internal/consultations/measurement-release_test.go`。

## 診断の履歴

成功しなかった呼び出しの診断は、使用量の保留に入らない場合（使用量は取れた時間切れなど）も、下書きの記録の `aiDiagnostics` に残る。再起動のあとも読める。最新20件までで、古いものから捨てる。成功した呼び出しの診断は残さない。各件に相関情報（実行先 `provider`・`model`・`effort`・工程 `step`・試行 `attempt`・ターン `turn`）と時刻 `at` を添える。診断の保存に失敗しても、呼び出しの流れは変えない。

## 相談の失敗の原因

相談の呼び出しの全体制限は通常5分。下書き工程で tier が heavy または effort が high の場合と、attempts が 0 より大きい場合は10分。無出力の制限は240秒。相談の難易度は依頼元が必ず指定し、受付が判定する呼び出し（旧「受付の始めの判定」）はない。

`timeout_idle` は使用量の確定・不明にかかわらず呼び出し内で再試行しない。相談本文は失敗として停止し、通常の失敗回数と連続時間切れ回数を増やさない。代理確認では使用量が確定していれば質問を利用者に回し、未確定なら使用量の保留で停止する。起票確認（close-review）、有事の期限切れの対処（ops-watch）、起票時の変更範囲見積もりも再試行せずエラーを返す。無出力の反復を避けるためである。

無出力時は既存の連続時間切れ回数を保持する。その後に通常の時間切れが起きた場合は、無出力前の回数から加算し、3回に達すると自動再試行しない。利用者の新しい返信では通常の受付処理により回数を初期化する。使用量確定後の予算計測結果の保存が失敗した場合も、無出力診断があれば通常の失敗回数を増やさず停止する。使用量上限への到達は次の予算検査で判定される。

相談の処理（Turn）の失敗は、下書きの記録の `lastFailure` に残す。`stage` は失敗した処理の段階、`cause` は元のエラーの文、`message` は `error` と同じ画面向けの文、`kind` は受付AIの呼び出しの失敗の種類（ある場合だけ）。

`cause` は `contract.MaskCredentials` で資格情報を伏せ、相談の会話・本文と一致する部分を除き、1行・500文字以内に縮める。資料の本文は照合していないので、エラーの文を作るときに資料の本文を入れない。AIの標準エラー出力は、これまでどおり記録しない（上の診断の範囲で切り分ける）。

公開の文を持つ失敗（`ai.Failure`・`contract.Problem`）は `error` もその文のまま。それ以外は `error` に段階と `cause` を添える。読み方は `GET /api/drafts/{id}`、`create-issue.ts --check`、`desk-alerts.ts`（`error` の範囲）。同じ原因を `相談の処理に失敗しました id=… stage=… cause=…` の1行でログにも残す。起票の失敗も同じ形で `lastFailure` に残す（`stage` は `起票`）。受付AIの呼び出しの前で相談が `thinking` のまま進めないときも、状態を変えずに `lastFailure` を残す。

既存の相談の、この変更より前の失敗の原因は復元できない。次に失敗したときから読める。

## 切り分けの見方

- `timeout_idle`: 出力期限が設定されている場合は、プロセス活動の有無にかかわらず出力期限中に出力がないと停止する。Claudeでは本文・推論・構造化回答の空でない途中増分も出力期限を更新するが、空イベントやキープアライブは更新しない。途中イベントは回答や使用量に採用しない。また、出力とプロセス群の読み込みバイト・CPU時間の増加が無活動期限中に検出されない場合も停止する。`/proc` を利用できない場合、無活動は出力だけで判定する。全体の制限時間と取消は途中増分が続いていても有効である。
- `timeout_total`: 活動の有無にかかわらず、全体の制限時間を超えた。
- `canceled_by_server`: 受付の更新（再起動）などで呼び出しが中断された。
- 呼び出しの途中で受付が再起動すると、完了の記録が残らず、`pendingCall` のまま使用量が不明になる。

テスト: `server/internal/ai/diagnostic_test.go`、`server/internal/consultationai/measurement_test.go`、`server/internal/consultationai/failure_cause_test.go`、`server/internal/consultations/failure-cause_test.go`、`server/internal/contract/failure_cause_test.go`。

## 無出力の強制終了の直前の記録

出力に加え、プロセス群の読み込みバイトまたはCPU時間の増加も活動として無出力の時計を戻す。値を比較できない場合は活動とみなさない。全体の制限時間は変わらない。

活動は無出力時間の4分の1（最大5秒）ごとに標本を比較して検出するため、実際に活動が止まってからの時間切れは最大で約1標本間隔遅れることがある。わずかな増加も活動とみなす。

標本取得の負荷はプロセス数や同時呼び出し数に依存する。運用環境で確認するときは、受付と `/proc` の PID 名前空間が一致することを確認し、同時呼び出し数、標本間隔、ホストのプロセス数、測定時間帯と配置版を記録する。同じ環境で呼び出しが少ない時間帯と多い時間帯の受付プロセスの CPU 使用率と応答時間を比較し、他の処理の負荷も併せて確認する。比較だけでは標本取得の寄与を確定できない。

`timeout_idle` の診断の `idleProcess` は、`/proc` から読んだ回数・量の数値だけを持つ。コマンドライン・パス・接続先は持たない。読めなかった値は 0 にせず null にする（`/proc` が無い環境と、`/proc` のプロセス番号が受付から見た番号と合わない環境では全部 null）。読み方は `GET /api/drafts/{id}` の `aiDiagnostics`。

- `processes`・`children`: プロセスグループの全プロセス数と、AI本体を除いた数
- `cpuMs`: グループがこれまでに使ったCPU時間
- `windowMs`: 最後の出力の後に読んだ基準（出力から5秒以内）から強制終了までの時間。基準を読む前に時間切れになると null で、次の3項目も null
- `cpuDeltaMs`: その間に使ったCPU時間
- `readBytesDelta`・`writeBytesDelta`: その間に読み書きしたバイト数（`/proc/<pid>/io` の `rchar`・`wchar`）。子プロセスが途中で終わって累計が減ったときは null
- `established`・`otherConnections`: グループが持つTCP接続のうち、確立中の数とそれ以外の数
- `txQueueBytes`・`rxQueueBytes`: 相手が受け取り確認をしていないバイト数と、受信したがプロセスが読んでいないバイト数

活動によって無出力の時計が戻された場合、`windowMs` は無出力時間より長くなり、増分には途中まで続いた活動も含まれる。

見方の目安（仕組みからの推定で、実例との照合はこれから）:

- `readBytesDelta` か `cpuDeltaMs` が増えている: AIの実行プロセスは動いていて、読み込みか計算をしている可能性がある。読み込みバイトにはファイル読み込みも含まれ、応答の受信とは断定できない。
- どちらもほぼ増えず、`established` が1以上: 接続は開いたまま、相手からの応答を待っている。
- `established` が0: 接続が切れた後も、実行プロセスが終わらずに残っている。
- `txQueueBytes` が残っている: 送信が相手に届いていない。

`timeout_total` には `idleProcess` は付かない。時間切れの調査では診断 ID、時刻、経過時間、最終出力時刻、出力バイト数を呼び出しの状況と照合する。`readBytesDelta` がある無出力記録でも、`rchar` だけからファイル読み込みと応答受信の内訳や過去の読み込み元を復元することはできない。切り分けに追加の観測が必要な場合は、内容や接続先を診断に保存しないよう運用環境で観測方法を選ぶ。

テスト: `server/internal/ai/idle_snapshot_test.go`。

## 起票確認の停止分類

起票前確認の同一改訂では、資料の読み足しは最大8回、判定は別枠で最大5回。読み足し上限後の資料要求は追加取得せず資料不足で停止し、判定1回として数える。求めた資料が容量上限（読み足し全体60000 bytes・入力全体の縮小）だけで収まらないときも、資料不足で止めず、追加取得なしの同じ最終判定を1回行う。件数上限・取得失敗・選択失敗・入力全体の超過は従来どおり停止する。`filingCheckCalls.budget` は両方の回数と結果待ちを記録し、`checks` の最終判定行は `refused`、全体の `outcome` は `evidence_requested`、`stopKind` は `evidence_missing` となる。予算記録のない履歴は各行の結果から回数を復元し、不完全な履歴（`count` だけ・行の欠落や切詰め・不正な値・結果不明）は上限到達として扱う。`count` は読み足しと判定を合わせた呼出し回数の記録で、予算記録（`budget`）がある記録では停止・通知・修復の判定に使わない。予算記録のない履歴では現在の改訂の行数との照合に使い、合わなければ上限到達として扱う。修復による初期化で0に戻る。過去の記録は書き換えない。

上限通知（`filing-check-limit-<相談ID>`）と修復による予算の初期化は、確認を止める判定と同じく、復元した予算で判定が上限5回に達し、結果待ちでないときだけ対象になる。読み足しの回数だけでは通知も初期化もしない。通知の題名と `detail` は読み足し回数と判定回数を分けて示し、`evidenceCalls` が読み足し、`decisionCalls` と `failures` が判定の回数を持つ。結果待ちの間、起票中・起票済み・破棄済みの相談、相談の改訂が記録と異なる場合は通知しない。コード版の変更だけでは予算は戻らず、通知の `codeRevisionChanged` で示す。資料不足の集約通知（同じプロジェクトで3件以上）は予算と独立に出る。

上限通知と資料不足の集約通知（`filing-evidence-missing-<プロジェクト>`）は `FilingCheckLimitAlerts` が返し、起動時に `server/internal/startup/business.go` の `draftStallAlerts` が相談処理の例外の通知と合わせて止まりの一覧（`GET /api/stalls/alerts`）へ渡す。画面の一覧・`scripts/desk-alerts.ts` は同じ一覧を読み、`kind` は `intake_error`、`status` は専用の `filing_check_stopped`（画面の表示は「起票前の確認を停止」）、`requiresUser: true` と `nextAction` を持つので要対応（次の担当は利用者）に並ぶ。`reason` と `detail` は利用者向けの説明、`outcome` は確認の結果（上限通知は `filingCheckCalls.outcome`、集約通知は `evidence_missing`）、上限通知の `draftId` は対象の相談、`detectedAt` は最後の判定の時刻を持つ。通知は読むたびに相談の記録から作り直すので、修復・相談の更新による再確認・起票・破棄で条件を外れると次の読取から消える。画面の非表示（`/api/desk-cleanup/close`、対象 `stall:<通知ID>`）は通知の内容に結び付き、回数・題名・最後の判定のコード版（`codeRevision`）が変わると再び出る。配置中のコードの版は通知に入れず、それとの比較の `codeRevisionChanged` は照合から外す（`server/internal/deskcleanup/records.go` の `AlertFingerprint`）ので、デプロイだけでは非表示の通知は出直さない。

修復による初期化は、修復の基点が有効な記録に1回だけ適用され、初期化の記録は次の呼び出しの予約と一緒に保存する。再読込や再送で修復権は戻らない。結果待ちの予約は結果不明として残し、修復でも未使用へ戻さない。相談の更新による再確認は新しい改訂の予算で行う。

停止した呼び出しの `filingCheckCalls.checks[].refusalCategory` は停止理由の診断用で、判定自体を変えない。`input_limit_only` はAIがそう申告し、本文必須の起票済み候補と件数上限を除いた入力上限の省略が記録されている場合、`other` はAIの `other` 申告または本文取得不能によるコード側の停止を示す。AIが `unknown` と回答した場合や旧形式で分類を返さなかった場合は `unreported`、申告が記録と合わない場合は `unknown` と保存する。変更前の `unknown` は未申告と不整合を区別できない。

既存の記録は書き換えない。旧版に戻しても新しい分類値を読むための変換は不要だが、旧版の新規記録には区別が付かない。

停止記録の `reportedRefusalCategory` は、コード側の補正前にAIが申告した分類（`none`・`input_limit_only`・`other`・`unknown`）をそのまま残す。旧形式で未申告の場合や許可された値以外の場合は項目を保存しない。これにより明示的な `unknown` と未申告を区別できる。変更前の記録にはこの項目がなく、過去の申告値は復元できない。追加資料として要求した起票済み依頼（`request:`・`job:`）の本文が資料入力上限で省略された場合も、`input_limit_only` の根拠に含めない。参照や発言だけの省略は対象外。

相談の下書きの呼び出し契機は、保存された最後の診断が同じ呼び出し回数に対応し、サーバー中断以外の失敗を示す場合を `failure_retry` とする。中断、診断がない旧履歴、最新の呼び出しに対応しない診断は `recovery_continuation` とする。入力記録があり呼び出し回数が0の履歴は不整合のため契機を記録しない。形式修復の対象は配備スキーマが定義するフィールドを一意に特定できたときだけ記録し、不明や複数の場合は省略する。

## 保存済み呼び出しの時間の比較

`GET /api/stats` は前後の期間を指定して使用量を返す。稼働実績のGET窓口（skills/issue-desk-stats/SKILL.md）で、例えば日本時間の2週間を比較するには次を実行する。

```sh
~/.issue-desk/desk desk-get.ts GET '/api/stats?tables=usage&beforeFrom=2026-10-01&beforeTo=2026-10-07&afterFrom=2026-10-08&afterTo=2026-10-14'
```

4項目は全て指定する。日付の終了日は含まれ、日時（`YYYY-MM-DDTHH:MM`）の終了は排他的で、各期間は最大92日。応答の `usage.before` と `after` の `intakeTiming` は、保存済みの `source=intake` 行を工程 `step`、実行先 `provider`、`model`、推論の強さ `effort`、難易度 `tier`、`diagnosticKind` の組で分ける。各組の `count` を比べると頻度を調べられる。同じ他の軸の `completed` と `timeout_idle` の件数と `elapsedMs`、`firstOutputMs` を比較する。各時間の `median`、`p90`、`missing` はミリ秒の中央値、90パーセンタイル、欠測件数。実測0は値として含む。中央値は偶数件なら中央2値の平均、P90は昇順で ceil(既知件数×0.9) 番目。既知値がなければ分位値は null。軸の欠測も null で、旧行の難易度・診断・最初の出力時間を推測しない。

対象は保存済み行だけで、保存に失敗した呼び出しは含まれない。診断分類のない旧行は成功や時間切れに分類できないため、この件数を全呼び出しの頻度とみなさない。

入力の `promptTotalBytes` はAIへの `request.Input` のUTF-8バイト長で、別ファイルから読むschemaは含まない。内訳の `schema` はそのファイルを別途読み取ったバイト数。入力の解析に失敗すると内訳は欠測になり、schemaを読めない場合も欠測となる。観測できない部分も0に補わない。

## 起票確認の入力縮小率

`GET /api/stats` の `filingCheck.inputReduction` は任意欄で、旧応答には存在しない。値は `null` または `before`・`after`・`coverage` を持ち、coverage は `retained_checks_only`。各期間の `known` は保持checkの `inputReduced` が真偽値の件数、`reduced` はそのうち true の件数、`unknown` は旧形式・未計測の件数。件数は非負整数で `reduced <= known`、`rate` は `reduced / known`、`known=0` では null。入力上限による縮小を数え、常時行う `reduceNonOverlapping` は数えない。AI呼出し前に上限停止しcheckを作らない経路は分母外であり、全呼出しや全停止の頻度ではない。

期間とprojectは既存集計と同じ `[From,To)` と選択条件を使う。両期間を比較するときは `codeRevision`、計測開始時点、保持条件、処理量の違いを記録する。対象0件（全件数0）、unknownのみ（known=0、unknown>0）、読取不能を区別する。前期間がunknownのみなら比較できない。既存イベントの30日保持は保持checkの保持条件ではない。指示文の約600bytes増加との因果や影響がないことは、この率だけでは断定できない。

片方でも保持checkを読めず指標を確定できない場合、`inputReduction` 全体を null とし、`unavailable` の文字列で `inputReduction`・対象期間・理由を識別する。0件やrate=0に置き換えない。既存のイベント集計 `before`・`after` とその `reasons` は独立しており、縮小率だけの取得失敗では表全体を失敗させない。

## 読み足しの配信量の記録

使用量行の `contextReads` は `requested`・`repeated`・`files` に加え、intake の観測として `deliveryBudget` を任意に持てる。指定時は読み足しの `round`、上限到達を示す真偽値 `limited`、未配信資料数 `omitted`、配信した本文のバイト数 `deliveredBytes` の4項目すべてが必須で、他の項目や null は受け付けない。数値は非負整数で、`round` と `omitted` は9007199254740991以下、`deliveredBytes` は160000以下とする。未記録は `deliveryBudget` 自体を省略し、0やfalseで補わない。

上限到達回数は `limited=true` の行をdraftと読み足しroundの組で重複排除して数え、資料数を表す `omitted` とは区別する。`limited=false` の観測行と `deliveryBudget` のない旧行も別に集計する。配信元が観測した値だけを送り、過去分を推測で埋めない。この保存契約自体は配信量の制限や観測値の送信を行わない。
