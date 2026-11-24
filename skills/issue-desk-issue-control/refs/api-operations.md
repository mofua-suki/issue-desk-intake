# Issue操作・チャット承認を汎用窓口で送る

## 接続・送信・結果確認

NASは既存の `sh ~/.issue-desk/desk-pc get` / `operation`、Ubuntu PCは `~/.issue-desk/desk get` / `operation` を使う。いずれもそのPCの既存K2のPC別トークンを使う。変更操作には運用の代行を許したトークンが必要。認証・権限を増やしたり一時キー・画面セッションへ切り替えたりして拒否を回避しない。窓口が使えない環境は [issue-desk-api](../../issue-desk-api/SKILL.md) の接続方法とAPI仕様を読み、同じ既存トークンで送る。

1. 送信前に共通番号をGET `/api/issue?desk=<共通番号>&view=summary` で照合する。応答の `body` のproject・数値number・title・`state`・`status` を確認する。要約にはこのほか `priority`・`host`・`pr`・`closedAt`・`closeReason`・`deletionApproval`・`permissionApproval` があり、対象の照合と停止・再開・担当変更・優先度の確認は要約で足りる。本文・会話、`retryable`、`pendingFinalApprovals` は要約に無いので、再試行と承認（削除・権限・最終承認）は `view` を付けない全文のGETで読む。`actions` はPC別トークンの応答には付かないので、欠測を理由に送信を止めず、可否は `state`・`status` と受付の拒否理由で確かめる。以後のJSONの `number` は応答のGitea番号で、共通番号ではない。GET失敗・照合する欄の欠測・不一致なら送らない。query値はURLエンコードする。
2. 初回送信前に小文字のUUID（8-4-4-4-12形式）を生成し、接続先・送り手の識別情報・対象・JSON本文とともに、ホーム下の別cloneの `.tmp/` に保存する。資格情報の値を保存しない。JSONの重複キー・未知項目・型違いを除き、文字列はJSONとしてエスケープする。以下の例の値は実対象に置き換え、例のIDを別の操作へ使い回さない。
3. 保存した本文ファイルを `operation` へ渡し、応答の `status` と `body` を読む。終了コード0だけで現在値まで確認できたと扱わない。操作後に同じ対象をGETし、操作に応じた状態・優先度・担当・承認記録を確認する（`view=summary` で足りる。未了の最終承認の一覧だけは全文のGET）。送信成功後のGET失敗は「送信済み・状態未確認」と報告し、POSTせず読取だけを再実行する。

```sh
sh ~/.issue-desk/desk-pc get --path '/api/issue?desk=4553&view=summary'
sh ~/.issue-desk/desk-pc operation --method POST --path /api/comment --body-file <ホーム下の作業ツリー>/.tmp/issue-operation.json
```

POSTの結果が不明なら新しいIDを作らず、同じ接続先・送り手・対象・保存済み本文で再送して受付に照合させる。成功が確認できているときは再送しない。400は入力、401は認証、403は認証区分・運用代行・送り手、409は対象・状態・ID衝突を調べ、応答の `retryable` / `safeToChange` に従う。拒否や読取失敗を成功・対象なしと扱わず、権限や状態を変えて回り込まない。5xx/通信失敗の同一要求照合と、条件が変わった後の新規操作を区別する。

正本仕様は `api/openapi/60-todos-hosts.yaml` の `/api/comment` と `/api/handover`。サーバーが状態・権限・同ID同内容を再検査する。

## 再試行

利用者が再試行を指示したとき、または失敗の原因を調べて解消したと説明できるときだけ送る。提案・承認依頼の前に、会話記録・使用量の内訳・レビュー指摘などから原因と今回変わる条件を確かめ、変わらなければ提案しない。「マージ後の運用作業」の確認が必要なら先に済ませる。同じ原因で繰り返さない。

初回は `view` を付けない全文のGETで読んだIssue詳細の `retryable === true` を確認する（要約には `retryable` も本文・会話も無い）。単なる `waiting` では足りず、使用量の上限を超えた質問待ちに限る。失敗・レビュー保留・前提待ち等も受付の可否を優先する。送信直前に本文と非logコメントを読み、有効な条件・取消し・原因と今回の変更を照合する。追加指示 `text` は8000文字以内で、実装担当への指示を入れ、利用者への報告を混ぜない。

POST `/api/comment` の通常の本文:

```json
{"id":"12345678-1234-1234-1234-123456789abc","project":"owner/repo","number":12,"action":"retry","text":"原因と今回の条件を踏まえた実装担当への追加指示"}
```

- 指示がなければ `text` は空文字。画像は付けない。
- 前回が固定テストの失敗だけで、原因が既定ブランチで既に直ったと説明できる場合だけ `retest: true` を追加する。固定テストだけ再検証し、不合格で実装へ切り替わった際にも `text` を引き継ぐ。
- 前回の変更を引き継がず既定ブランチからやり直す明示的な場合は `fromBase: true`。`retest` と併用しない。既定でfromBaseやretestを付けない。
- 使用量・実装回数の上限を超える再試行は、利用者がその依頼のその再試行を明示的に承認した場合だけ `overLimitStatement` と `instructionAt` を追加する。発言は下の承認規約に従い、対象の共通番号かGitea番号を数字列として含める。1回の発言は1回だけ、別の操作へ再利用しない。`text` に止まった原因と今回変わる条件を空白以外20文字以上で書く。`fromBase`・画像・自動指定とは併用しない。承認を時間切れやAIの判断で代えない。
- 期限付きのプロジェクト許可 `implementation-limit-retry` を使う明示的な再試行は `implementationLimitRetry: true` と原因・直し方を含む空でない `text` を送る。有効な許可が事前に必要で、1依頼につき許可更新を跨いで累計2回まで。`retest` は併用可、`fromBase`・`overLimitStatement`・画像・自動指定は併用不可。許可を自分で作ったり期限切れを更新して回避したりしない。
- `instructionAt` は `overLimitStatement` を付けるときだけ指定する。通常retryへ不要な承認項目を付けない。
- 同ID再送は既に受理されて `retryable` がfalseでも、保存済み同一本文を受付で照合する。新規の再試行と混同しない。

明示的に送信間隔を約束した系列は、同一ホスト・ユーザー・系列ID・間隔で `desk-paced.ts` を使う。初回待機なし、次から前回終了後に指定間隔を待ち、再開時も全時間待つ。系列変更で待機を避けない。

```sh
sh ~/.issue-desk/desk-pc desk-paced.ts --sequence corrections-20261113 --interval 180 -- operation --method POST --path /api/comment --body-file <ホーム下の作業ツリー>/.tmp/retry.json
```

NASでもこの呼出しはPCトークン窓口を明示し、Go製 operation と同じK2を使う。Ubuntuでは `~/.issue-desk/desk desk-paced.ts` を使う。

`state.json` の終了と単調時計で計った待機を確認する。壊れた記録・終了不明・所有者不明の排他を自動解除しない。保存した送信IDで実状態を照合する。

## 停止・再開・担当変更

使えるのは利用者がその操作を指示した場合、または [issue-desk-operations](../../issue-desk-operations/SKILL.md)「起票する」のエージェントによる割り込み条件に当てはまる場合だけ。範囲を推測して広げない。

停止・再開はPOST `/api/comment`。`action` を `stop` / `resume`、`text` を空文字にする。再開に理由を添える場合は、前後の空白を除いた1〜8000 UTF-16コード単位の `text` で送る。変更範囲の上限で止まった依頼では、範囲を広げる理由を書く。停止や優先度操作へ `text` の追加指示を入れない。

```json
{"id":"12345678-1234-1234-1234-123456789abc","project":"owner/repo","number":12,"action":"resume","text":"範囲を広げる理由"}
```

停止は動いている/動きうる依頼だけ、再開は `status` が `stopped` の依頼だけ。要約の `state`・`status` と受付の拒否理由に従う。

担当変更はPOST `/api/handover`。開いていて作業が始まっていない依頼だけ。`host` は担当表のホスト名（英小文字/数字で始まる英小文字・数字・ハイフン、31文字以内）を指定する。受付がラベルを付け替えて引継コメントを投稿する。POST成功は引継先での着手完了を意味しない。

```json
{"id":"12345678-1234-1234-1234-123456789abc","project":"owner/repo","number":12,"host":"gt12"}
```

## チャット承認の共通規約

削除・権限・未了の最終承認と相談の続行等は、利用者がチャットで対象と操作を特定して明示的に了承したときだけ送る。曖昧な「進めて」、別対象の了承・AIの推測/要約・時間切れでは送らない。運用代行PCトークン専用で、一時キー/画面セッションへ切り替えない。

`statement` には利用者発言の原文（前後の空白を除く）を入れる。8文字以上で「承認します」等の定型語だけにしない。記号で答えた場合は、利用者に見せた質問文と回答から次の形を作り、発言を頼み直さない。

`質問「<見せた質問文そのまま>」への利用者の回答: <答えた記号そのまま>`

質問文には対象の番号と操作を含め400文字以内、記号は `A`・`1-A`・`はい` 等の実際の回答。AIが質問・回答を創作しない。10件超の一括操作は件数・番号範囲・選択条件で聞き、実行直前の再読取で条件から外れたものを飛ばす。

`instructionAt` は、その発言/回答の会話上の日時をUTCオフセット付きISO 8601（秒まで）で指定する。未来や7日より前は不可。送信時刻を利用者発言の時刻として補わない。原文・日時・対象が揃わなければ送信を保留する。サーバーが発言・日時・経路・送信元PCを記録する。

### 削除・権限の承認

`view` を付けない全文のGETで詳細の `deletionApproval` / `permissionApproval` と `state`・`status` を読み、対象headと完全な対象集合を利用者の承認と照合する。`unavailable`・欠測・head変更なら送らない。headは小文字40桁。削除は `action: "approve-deletions"`、権限は `action: "approve-permissions"` をPOST `/api/comment` に送る。

```json
{"id":"12345678-1234-1234-1234-123456789abc","project":"owner/repo","number":12,"action":"approve-deletions","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","statement":"共通番号4553の対象の削除を承認してマージして","instructionAt":"2026-11-11T11:58:00+09:00"}
```

同じ構造で権限はactionを変え `permissionApproval.head` を使う。承認はそのheadと対象集合だけに効き、変われば旧承認を流用しない。成功すると実装AIを起動せず公開の続き/固定テスト・独立レビューから再開する。監査コメント未完了の503は同じID・本文で続きから再送する。他の保留理由は残る。直接PRの権限承認はこの経路へ送らず [merge-manual-pr](../../merge-manual-pr/SKILL.md) に従う。

### 未了の最終承認

`view` を付けない全文のGETで詳細の `pendingFinalApprovals`（要約には出ない）の `id`・`headSha`・`fingerprint` を読み、承認IDとheadが利用者へ提示した対象と一致することを確かめる。欠測・`finalApprovalsUnavailable`・ID/head変更なら新規送信しない。質問文には共通番号・承認ID・40桁head・了承する操作を含め、1承認ずつ聞く。statementは8000文字以内で、承認IDを完全な語として（前後が英数字・`_`・`-`でない）とhead全文を含める。

POST `/api/comment` に共通番号の数値 `desk`、または `project` と数値 `number` のどちらか一方を入れる。

```json
{"id":"12345678-1234-1234-1234-123456789abc","desk":4553,"action":"approve-final","approvalId":"deployment_both_sha","head":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","approvalFingerprint":"<取得したfingerprint>","statement":"質問「共通番号4553の承認 deployment_both_sha（head SHA: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa）を了承しますか？ A: 了承／B: 保留」への利用者の回答: A","instructionAt":"2026-11-11T11:58:00+09:00"}
```

結果不明の同ID再送は、既に記録済みで未了一覧から消えた、またはheadが進んだ場合も、元の承認ID/head/発言/日時を変えず受付の受領記録と照合する。この場合 `approvalFingerprint` は空文字でも照合できるが、新規送信の照合省略に使わない。

### 相談の続行・代理回答取消・組の了承・プロジェクト作成

送信前にGETで現在の対象を読み、利用者へ示した対象と一致することを確かめる。相談ID・プロジェクト相談IDは小文字UUID。次の各APIのJSONには共通規約の `statement` と `instructionAt` を必ず追加する。

| 操作 | POST先 | その他のJSON項目 | 結果不明時 |
| --- | --- | --- | --- |
| 呼出し上限後の続行 | `/api/attention/consultation-budget/<draftId>/continue` | `operationId`: 固定した送信ID、`stopCount`: 現在の正の整数 | 同じID・停止回数・本文で照合。停止回数を勝手に更新しない。 |
| 代理回答取消 | `/api/drafts/<draftId>/proxy-undo` | `messageId`: roleがproxyの対象発言ID | 相談を読み直し取消結果を確認してから判断。無条件再送しない。 |
| 組の変更・除外例外への了承 | `/api/drafts/<draftId>/chat` | `messageId`: 固定した小文字UUID、`text`: `了承します`、`approvalFingerprint`: 現在の `crossProposal.fingerprint` | 同ID・本文で照合。指紋が変われば再確認。 |
| 新プロジェクトのリポジトリ作成同意 | `/api/projects/<consultationId>/approve` | `fingerprint`: 確定定義の現在の指紋 | 相談を読み直し作成結果を確認してから判断。無条件再送しない。 |

続行は停止した相談に呼び出し20回・重み3000000を追加する操作。代理回答取消は元の質問を利用者回答待ちへ戻す。組の了承はGET `/api/drafts/<id>` の指紋、新プロジェクトはGET `/api/projects/<id>` の指紋をそのまま使い、変わった定義に承認を流用しない。
