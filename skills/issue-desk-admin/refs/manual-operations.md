# 手動運用を汎用API窓口で行う

[共通API手順](../../issue-desk-api/SKILL.md) に従い、NASは `sh ~/.issue-desk/desk-pc get|operation`、Ubuntuは `~/.issue-desk/desk get|operation` を使う。以下は運用代行を許したPCトークンの経路。認証・所有者・対象プロジェクト・状態の制約は受付が検査する。403で別経路へ回り込まない。

利用者が対象と操作を指示した場合、または記録・決まりから対処済み・対処不要と確かめられる場合だけ実行する。記録のない推測や経過時間で許可を代えない。送信前に対象と現在の状態を読み、許可されるactionsと照合する。応答 `body` と操作後のGETで結果を確認し、HTTP成功だけで後続処理の完了を報告しない。

## 要対応を確認済みにする

| 対象 | POST先 | 本文と条件 |
| --- | --- | --- |
| 利用量の見直し | `/api/usage-review/rounds/<正の整数>/acknowledge` | `{}`。原因と対応を確認した回だけ。 |
| 運用の見張り | `/api/ops-watch/notices/<通知UUID>/acknowledge` | `{}`。対応済み・対処不要の根拠を確認する。 |
| 有事 | `/api/incidents/<有事UUID>/acknowledge` | `{}`。対応・解消・期限切れの後だけ。「自分で対応する」は画面専用で代行しない。 |

返されたid、round、acknowledgedAt、stateを対象に応じて確認する。確認済みにすることと原因修正・再開は別。受付側の見張りの期限切れ通知など、消すAPIがないものを消したと報告しない。

## 相談の終了・破棄・TODOへ戻す

対象UUIDをGET `/api/drafts/<id>` で読み、親子・処理中・起票済み・後続の有無と利用者の意図を確認する。instructionAtは実際の指示の会話日時（オフセット付きISO 8601、実在し未来でない日時）。利用者の実行時刻やAIの判断で埋めない。

- 記録を残して起票せず終える: POST `/api/drafts/<id>/finish`、本文 `{"instructionAt":"<指示日時>"}`。未起票の下書きや終了可能な保存済み調査の相談が対象。結論が「起票しない」・調査終了等と記録から確認できる場合だけ。分割の親子・AI処理中・起票中・起票済みは受付が断る。
- TODOへ戻す: POST `/api/drafts/<id>/todo`、本文 `{"instructionAt":"<指示日時>","reason":"<必要時の理由>"}`。迷ったときの退避先。元の相談状態と返されたTODOのidを照合する。
- 相談を破棄する: DELETE `/api/drafts/<id>?instructionAt=<URLエンコードした指示日時>&reason=<必要時の理由>`。元に戻せないため、対象を特定した利用者の指示、または同じ内容が起票済み/TODO済みと記録で確かめた場合だけ行う。本文JSONをDELETEへ送る形式ではない。終了・TODOへの退避と混同しない。

通信断では同じ相談をGETして結果を照合し、無条件に再送しない。409で現在の版・状態・指示日時より後の変更を確認し、古い意図を変わった対象へ流用しない。利用者に画面のボタンを1件ずつ押させない。

## TODOの一覧・保存・削除

一覧・既存本文の保存・相談開始は [TODO手順](../../issue-desk-operations/refs/todo-edit.md) を読む。本文のIssue番号の照合、revision・画像保持・送信IDを省かない。作成途中の空TODOだけで保存成功とせず、保存と相談開始を区別する。

削除はGET `/api/todos/<id>` で現在のrevision・本文・期限・条件を照合し、POST `/api/todos/<id>/remove` に `{"revision":<取得した数値>,"instructionAt":"<指示日時>"}` を送る。利用者の指示、または記録から不要と確認できるときだけ。操作後の結果を確認し、版の409を無視して削除しない。結果不明では先にGETで照合する。

## hostdへ版の指示を出す

まずGET `/api/hosts/<PC名>/hostd-command` でpin・command・resultを読む。欠測・不正応答を「指示なし」「成功」としない。install/rollbackは利用者の指示、またはGET `/api/hosts` で復旧が必要と確かめたときだけ。pinは利用者の指示があるときだけ、解除はfollowを使う。OS再起動やSSHの許可を兼ねない。

送信前に32桁の小文字16進IDを生成し、本文を保存する。POST先は同じ `/api/hosts/<PC名>/hostd-command`。

```json
{"id":"0123456789abcdef0123456789abcdef","kind":"install","tag":"abcdef012345-python","ttlSeconds":3600}
```

kindはinstall / rollback / pin / follow。install・pinのtagは発行履歴にある12桁小文字16進と任意の `-python`。rollbackのtagはprevious（省略時）またはlast-good、followにはtagを付けない。ttlSecondsは整数60〜86400（省略時3600）。1台の有効な未実行指示は1件、別IDは409。同ID同内容の再送はcreated=falseで照合し、別内容へIDを使い回さない。

指示発行は完了ではない。同じGETで未実行commandのid・期限とresultのid・kind・ok・reason・at、必要時logTail、pinを確認する。失敗版を飛ばしているPCへの同じ版installは、利用者の入れ直し承認に基づくhostd-repairの `PC名:retry-failed-image` が事前に必要。許可は [付与手順](../../issue-desk-issue-control/refs/grants.md) に従って必要最小TTLで登録し、用後に取り消す。失敗記録を消すためだけのinstallを自動で繰り返さない。

## 拒否された後続案の投稿し直し

利用者が対象の投稿し直しを指示したときだけ使う。元Issue・コメント・拒否記録と、採用済みの案がないことを照合してからPOST `/api/proposals/repost` へ送る。

```json
{"project":"owner/repo","number":12,"commentId":34,"instructionAt":"2026-11-13T10:00:00+09:00"}
```

numberとcommentIdは正の安全な整数。現在の拒否理由・対象PC・checkedAtと指示日時を照合する。未完了queued/ackedは同じ指示を再利用するが、failed/doneは採用記録がない場合に置換できるので、結果不明時に無条件再送しない。返されたcommandId・targetHost・statusを保存する。doneは監視のoutboxへ積まれた意味で、起票完了ではない。相談・依頼の一覧と元Issueで起票結果を確認する。詳細は [後続案の設計](../../../docs/followup-proposals.md#reposting-rejected-proposals) を読む。
