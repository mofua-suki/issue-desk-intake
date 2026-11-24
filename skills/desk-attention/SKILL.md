---
name: desk-attention
description: issue-deskの受付画面の要対応を見張るときに使う。種類ごとの元の経路・画面の判定・消える条件と操作手順、読み取り失敗の扱い、利用者の判断が必要な操作を確認する。
---

<!-- skill-standard: 1 -->

# 受付の要対応を見張る

「要対応」「実行中」「待機中」「停止中」の定義と画面の件数だけを確認する場合は [desk-list](../desk-list/SKILL.md) を使い、`GET /api/list?view=counts` で読む。このSkillの対処集計や監視記録の件数で代用しない。

接続・Issue番号の照合は [issue-desk-operations](../issue-desk-operations/SKILL.md)、要対応の操作の条件は [issue-desk-admin](../issue-desk-admin/SKILL.md)、有事の判断は [チャットの常設指示](../../chat-instructions.md) を参照する。このSkillのコマンドは正本のissue-deskで実行する。NAS への配布は `chat-deploy.json` の一覧を使い、`deploy/nas/update-skills.sh` が行う。

## 4分類と診断記録の違い

画面の要対応は具体的な利用者判断だけ。対処不能な引継ぎ・失敗・監視記録は、実行の根拠と自動再開経路により実行中・待機中・停止中へ分ける。`desk-alerts.ts` の `items`・`cases` は原因別の対処記録（引継ぎを含む）であり、画面件数ではない。画面の4分類の定義・件数・親子と関連通知の扱いは [desk-list](../desk-list/SKILL.md) と件数APIを正とする。以下の診断手順の集計値を画面と読み替えない。

## 分類と記録の取得条件

画面の分類の正本はdesk-listで示す共通分類。ここでは原因別の対処記録を読む。画面は `client/src/ui/request-list.ts` の `requestGroupsWithAux` と `client/src/ui/request-classification.ts` で分類する。CLIは7経路を読み `shared/src/desk-attention.mts` の対応する分類を使う。Issueは対応中の範囲（`/api/issues?scope=active`。画面の `/api/list?tabs=active` と同じ絞り込みで、終わった依頼を読まない）だけ、相談は submitted を除いて全ページ、クローズ依頼も全ページ読む。診断と画面では集約単位が異なる。並行取得は同一時点のスナップショットを保証しない。画面が欠測時に保持する旧データも比較条件に含める。

- 7経路の行を共通分類し、`items` は利用者の判断と原因別の対処不能な引継ぎ、`monitoring` は受付側の見張りが扱う記録として返す。通常の実行中・順番待ち・クローズ確認中の行は監視記録に含めない。完了・クローズ済み・フォローアップはそれぞれのタブへ分ける。
- Issueの質問待ち（実装回数・使用量の上限判断を含む）、未了承認、現在のレビューの利用者判断、削除・権限承認、停止解除・未着手のうち具体的な質問があるものを判断待ちとして出す。相談は質問がある下書き・質問待ち・調査結果の相談、クローズ依頼は確認待ちを出す。起票済み相談はIssueへ置き換え、調査Issueを持つ相談は両方の記録を残す。分割の子は質問待ちを要対応、失敗・停止を監視へ残し、親相談IDを保持する。親相談は画面の一覧に表示・計上せず、独立した子の仕事だけを表示する。子未作成の分割案は親と推測して隠さない。診断CLIのdesk-alertsは親自身の失敗理由も保持し、画面の件数と診断行数は同じとは限らない。
- 失敗・止まり・前提待ち・質問のない下書き・利用量の見直し・テスト結果・未知の種類は、判断の必要性が明示されない限り監視へ残す。承認情報はID・対象head SHA・理由を保持する。自動復旧の対処不能な通知は、JudgeのrequiresUserとは別のhandoffで原因別に引き継ぐ。分類や記号回答は個別の認証・承認を代替しない。
- 明示停止から24時間以上経過した相談は `/api/attention/alerts` の `consultation_stop` として診断の引継ぎ（items）へ出し、画面では停止中に分類する。停止理由・操作者・停止日時・経過を読み、解除条件を確認する。時間切れ停止・時刻不明は対象外。返信・再開・終了・破棄で停止状態を離れると消える。表示されたこと自体は停止解除の許可ではなく、自動では再開しない。
- 許可外プロジェクトは受付の一覧取得で除外する。トークンの相談一覧は `draftPermitted`（プロジェクト未定は残す）、クローズ依頼一覧は `projectPermitted` で絞る。補助データも共通分類し、判断と引継ぎを診断のitemsにする（画面では引継ぎを停止中へ分類）。未知の種類は監視へ残す。

| 種類・元の経路（GET） | サーバーの取得条件（取得後に共通分類） | 消える条件・消すためのスクリプト |
| --- | --- | --- |
| Issue `/api/issues?scope=active` | 許可プロジェクトの依頼とjobsの状態を対応中の範囲で取得。閉じた依頼は、実行・公開・配備の途中、継続待ち、受入条件の確認が未了、読んだ相談が起票結果・調査Issueとして記録、処理中のクローズ依頼の対象のどれかに当たる行だけ残る。scope を知らない古い受付は全件を返す。質問・承認・上限判断はitems、判断不要の失敗や前提待ちはmonitoringへ分類する。 | 回答・再試行・再開などで対象の状態を離れる、完了・クローズ済みになる。回答は `scripts/reply-issue.ts`、原因を解消した失敗の再試行・停止・再開は [Issue操作手順](../issue-desk-issue-control/refs/api-operations.md)。一律の確認済み操作はない。 |
| 相談 `/api/drafts` | 共通分類で質問待ちはitems、失敗・停止・質問のない下書きはmonitoringへ分ける。分割の子も同じ判断で子ごとに出し、desk-alerts のJSONとテキストにも parentSplit の親相談IDを残す。 | 回答して処理へ進む・起票する・終える・破棄する・TODOに戻す。回答は `scripts/create-issue.ts --draft <下書きID> --text "<回答>"`。終了・取り下げはissue-desk-admin Skillの手動操作。error・stalled の再試行は専用CLIがないため画面で行う。 |
| クローズ依頼 `/api/close-requests` | 一覧は `project`・`status`・`limit`・`page` で絞り、`hasMore` が偽になるまで読む。confirm が close_confirm として要対応。対象がフォローアップならそのタブへ分ける。 | close・keepの判断後、確認待ちを離れる。issue-desk-admin Skillのクローズ依頼判断API（close / keep）。 |
| 利用量の見直し `/api/usage-review/alerts` | verdict.result が not_recouped で acknowledgedAt がない回。 | 確認済みになる。issue-desk-admin Skillの利用量見直し確認済みAPI。 |
| main の全部のテスト `/api/full-test/alerts` | 最後の対象結果が failed。未解消のmain破損の件がある間はそちらの行へまとめる。`fullTestRows` は id=full-test の行だけ採る。 | 後の成功や失敗の解消で出なくなる。確認済みにするCLIはない。修正依頼は `scripts/create-issue.ts` で受付へ送り、テスト結果の更新は実行係に任せる。 |
| 止まり `/api/stalls/alerts` | Issueの止まり、パイプラインの止まり、監視停止・再起動、CLI版・hostのずれをまとめる。未解消の止まり、古いheartbeat、直近24時間の再起動・gaveup等。 | 止まりの解消・新しいheartbeat・版や自己検査の回復で消える。再起動・gaveupの行は24時間で消える。確認済みCLIはない。`~/.issue-desk/desk doctor` で診断し、Issueの停止・再開が必要なら control-issue を使う。 |
| 自動処理 `/api/attention/alerts` | 登録済みプロジェクトの開いている failed・review_held の件を、段「利用者の判断が必要」（notify）と理由「自動起票は停止されています。記録を確認して具体的な修正を依頼してください。」付きで出す。受付は調査・再試行・修正の依頼を自動では出さない。保存済みの記録は元の段のまま出る。 | 保存済みの retried・fix の件は left になると消える。任意に消すCLIはない。元Issueの原因の解消・再試行・回答で対応し、修正が要れば `scripts/create-issue.ts` で受付へ送る。「止まっている」印があれば原因を調べて対応する。 |
| `dependency_stall`（同じ `/api/attention/alerts`） | 先行ごとに、24時間超または待ち始め不明の依存待ちをまとめる。先行の状態と後続の共通番号を示す。 | 先行Issueの次の担当と対処を確認する。待ちが解けると自動で消える。 |
| 高の手戻り率（同じ `/api/attention/alerts`） | 手戻りの内訳による判定。直近14日の高のマージが3件以上、手戻り率が0.5超で1件。 | 確認済みの操作はない。率が0.5以下・件数不足・集計不能になると次の更新で自動で消える。 |
| `main_broken`（同じ `/api/attention/alerts`） | 登録済みrepoで cleared・resolved 以外の、保存済みのmain破損の件。受付は新しい件を作らず、確認・修正の依頼も出さない。 | 受付はこの記録を書き換えないため自動では消えない。消すCLIはない。main の今の結果を全部のテストの行で確かめ、修正が要れば `scripts/create-issue.ts` で受付へ送る。 |
| `ops_watch`（同じ `/api/attention/alerts`） | 運用の見張りの保存された通知。Issueの無い通知も出る。 | 確認済みまたは既存の自動整理で消える。issue-desk-admin Skillのops_watch確認済みAPI。通知だけを確認済みにし、Issue・ジョブ・相談は変えない。 |

## 見張る手順

`desk-alerts.ts` の `--project`、`--kind`、`--exclude-project`、`--exclude-kind` は繰り返し指定できる。project と kind は7経路のitemsとmonitoringに適用し、値が欠ける行は包含指定に一致せず、除外指定では残る。kind は各行の `caseKind` があればそれ、無ければ `kind` に完全一致する（自動処理の行は API 行の `kind` が `caseKind` になる）。API の `kind` は自動処理の行だけを絞る。各包含条件内は OR、除外が優先される。JSON の `filters` は適用中の指定を示す。空値・前後空白・制御文字・不明な query は 400。例: `node scripts/desk-alerts.ts --json --exclude-project igs`。

`node scripts/desk-alerts.ts --user-only` は絞り込み後の承認・上限判断待ちを一覧にする。読取失敗は理由を表示し終了コード1となる。`--json` と併用すると `--user-only` の有無によらず、既存フィルターを適用した既存JSONをそのまま返す。

1. `node scripts/desk-alerts.ts --json` を読む。別PCでは `--url <受付のURL> --token-file <トークンのファイル>` を加える。運用の代行を許されたトークンを使う。同じ結果の `items`（利用者の判断・引継ぎ）と `summary.screenTotal` を確認する。`monitoring`（失敗・止まり・未完成の相談・未知の種類など）も全件読み、`cases` の対象・各行の状態・自動処理の段と最後の動きを確認する。テキストでも先頭に診断用の行数、続いて全行を表示する。同じ原因の引継ぎは1行にまとめ、`attentionMembers` に元の表示項目を保持する。待ち・失敗の記録は判断の有無に応じてitemsとmonitoringへ分かれる。`summary.rawTotal` はitemsと同じ表示行数、`summary.total` は待ち・停止を分けた対処集計の行数、`summary.cases` は対象別の集約件数である。ops_watch は通知ID（keyの ops-watch: を除いたUUID）を使う。
2. failuresと終了コードを確認する。読めない経路があれば終了コード1になり、ほかの取得結果は残る。読み取りに失敗した経路を「要対応なし」と扱わない。取得済みの項目を調べ、未確認の経路・理由を報告する。403を別の認証やGitea直接操作で迂回しない。
3. 必要なIssueの内容は issue-desk-issue-control Skillの汎用GET、相談は受付APIの `GET /api/drafts/{id}` で確認する。PCの状態は [ホスト情報](refs/hosts.md) の汎用GET窓口、止まりの原因は手順1の desk-alerts の `monitoring[].detail` を先に読む。版・heartbeat・hostd周期結果・修復記録を取得時刻・欠測と照合して判断し、必要なら `~/.issue-desk/desk doctor` で診断する。原因と対応済みの記録を照合し、下の振り分けに従う。操作後は再読取し、実際に状態が変わるまで消えたと報告しない。

4. 報告は診断用の `summary.screenTotal` から書き、対処集計の `summary.total` と `summary.cases` を区別する。絞り込み時は `filters` と対象範囲を併記する。行を絞った結果で「無い」「新しいものは無い」と言わない。`summary.partial` がtrueなら取得できた範囲の暫定値と伝える。ページ途中やIssue詳細の照合失敗も欠測として理由を示す。`items` と `monitoring` の全行を確認し、監視記録の待ち・停止と `cases` の対処対象を区別して「手を打ったもの／何待ちか／利用者の操作が要るもの」に振り分ける。「利用者の操作が要るもの」は `nextActor=user` の件だけにし、`nextAction`・`evidence`・`url` を添える。記録の無い件を利用者の操作待ちにしない。`ai` はAIが調べて対処し、`system` は受付の進行を確認する。
5. 「止まっている」の印（`cases[].stale`）の付いた件は自動処理に任せず、原因を読んで手を打つ。基準は `--stale-minutes <正の整数>`（既定30分）。段だけを見て動いていると判断せず、最後の動きと自動処理が最後に行ったこと・時刻を確かめる。操作の条件は下の振り分けに従う。

clientの自動の運用の見張りは desk-alerts と同一の取得範囲とは扱わない。

## 処理の振り分けと利用者の判断

- 答えられるIssue・相談の質問は記録を読んで回答する。失敗の再試行は利用者が指示した場合か、原因を調べて解消したと説明できる場合だけ。原因が同じまま繰り返さない。回数上限や拒否は受付の判定を尊重する。
- Issueの停止・再開は [汎用窓口のIssue操作](../issue-desk-issue-control/refs/api-operations.md) のstop/resume。利用者の指示か [issue-desk-operations](../issue-desk-operations/SKILL.md)『起票する』の割り込みの条件が必要。desk-opsで再開しない。
- 確認済み・クローズ依頼の判断は、利用者の指示か、原因に対処済み・対処不要と記録から判断できる場合だけ。クローズは [issue-desk-issue-control](../issue-desk-issue-control/SKILL.md) の条件を守る。通知の確認済みを原因の修正と混同しない。
- 起票しない結論が記録で確認できる相談の終了・破棄・TODO退避は issue-desk-admin Skillの手順を使う。一部の子が起票済みで、残りの子が起票前で処理中でない分割は、親の終了で終える（破棄はできない）。分割の親子・処理中など受付が拒否するものは操作を重ねない。
- 削除・権限・未了承認、相談上限の続行（consultation_budget）、代理回答の取消、新しいプロジェクトの作成同意は [チャット承認の汎用窓口手順](../issue-desk-issue-control/refs/api-operations.md) に従う。利用者が対象を特定して承認した発言の原文（記号回答は表示した質問と回答）と、その発言の日時 `instructionAt`（オフセット付きISO 8601）が必須。複数プロジェクトにまたがる案の了承はその相談へ返信する。error・stalled の相談の再試行はIssue用のretry/stop/resumeと別操作で、API仕様と認証境界を確認する。利用者の承認をAIの判断で代えない。
- 有事の15分の期限の判定と期限切れの対処は、受付（NAS）の周期が行う。チャットのAIは、有事の記録（要対応の経過）を読んで続け、対処を重複して実行しない。利用者の返答は受付の要対応の「自分で対応する」で受け付ける。時間切れで代えられるのは承認だけ（操作条件は [チャットの常設指示](../../chat-instructions.md)）。上の利用者専用の操作（削除の承認・新しいプロジェクトの同意・複数プロジェクトの了承・代理の回答の取り消し）、明示された停止の解除、権限・許可一覧の追加は時間切れでも行わない。
- 品質・実行先・費用の方針、認証の発行、管理権限が必要な対応は利用者へ理由と対象を示す。単なる待ち時間で停止や再試行を決めない。

## 複数の依頼に広がる詰まり

### 見分け方

- 同じ失敗の文言（テスト名・メッセージ）が別々の依頼に2件以上出たら、依頼ごとに再試行しない。main・環境・監視側の原因を1か所で調べる。
- マージ済みの修正の後も同じ失敗が続くときは、次を比べて、その修正が各PCに届いているかを確かめる。
  - `GET /api/hosts`の各PCの `imageRevision` と main。
  - 共有DBの `host_image_results` の最後の報告。
  - 共有DBの `client_release_history` の発行。
- 同じ依頼で実装の時間切れが続いたら、再試行せずに分け直す。

### 直し方

- 原因を直す依頼は1件だけ出す。パイプラインを止めていれば、理由を添えて割り込みにする。
- 原因を作り込んだ依頼が別にあれば、その依頼には「main の版を取り込む」と返信し、二重の実装を避ける。
- 届けば直る失敗は、修正が各PCに入ったことを確かめてから、まとめて再試行する。
- 手作業（引き取りの停止など）を回避策にしない。仕組みの修正と、要るなら1回だけの復旧手順にする。
- 直す依頼には必ず prevention を入れ、同じ詰まりを要対応やテストで検知できるようにする。

### 読む手段

- PCの状態は、先に [ホスト情報](refs/hosts.md) の `GET /api/hosts` で読む（通常のPC別トークン、運用代行許可不要）。
- 止まりの原因は、先に `node scripts/desk-alerts.ts --json` の `monitoring[].detail`（JSONの止まり診断）で読む。運用の代行を許したトークンが要る。
- 版・heartbeat・hostd周期結果・修復記録を取得時刻・欠測と照合して判断する。読取失敗・不正応答・理由付きの空一覧・古いheartbeatを正常や待ちなしと断定しない。取得できたPC情報と旧調整記録由来の reason を区別する。
- `host_image_results` と `client_release_history` の全履歴はホスト一覧では返らない。必要情報をスクリプトで取得できず、既存の権限・運用条件でも許される場合の最後の手段として、共有DBをNAS上で読み取りだけのSQLで読む（`docker exec issue-desk-db psql -U issue_desk -d issue_desk -c "SELECT …"`）。書き込みのSQLは流さない。401・403の迂回には使わない。
- AIの判断で実行してよい範囲は、このSkillの「処理の振り分けと利用者の判断」に従う。

## 未採用案の整理

元Issueを閉じても残る後続案は [未採用・走査除外の整理](refs/proposal-dismissal.md) に従い、元コメントと現在の指紋を照合して理由付きで終了する。
