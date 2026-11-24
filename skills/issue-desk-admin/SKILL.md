---
name: issue-desk-admin
description: どのプロジェクトでも、要対応と回答待ちの相談の確認・対処、実行先の一時的な無効、モデルの固定規則の確認、PCの新しい引き取りの停止・再開を受付経由で行うときに使う。操作条件は各節に従い、Giteaを直接操作しない。
---

<!-- skill-standard: 1 -->

# issue-deskの受付経由の運用管理

接続・PCごとの起動用ファイル・トークンの許可範囲は [issue-desk-operations](../issue-desk-operations/SKILL.md) の「共通の決まり」に従う。本文を送る前の照合も同Skillの「送信前のIssue番号確認」に従う。

上限超えの再試行は、利用者がその依頼の再試行を明示的に承認したときだけ行う。再試行を提案する前に止まった原因と再試行で変わることを確かめ、変わらなければ提案しない（下記 issue-desk-issue-control の「再試行する」を参照）。承認発言は推測・要約で作らず、そのままを使う。運用の代行トークン・1回限り・再利用不可などの条件とコマンド例は [issue-desk-issue-control](../issue-desk-issue-control/SKILL.md#再試行する) の「再試行する」に従う。

## 要対応と回答待ちの相談を確かめる

利用者が求めたとき、または要対応の対処を頼まれたときだけ、下の desk-alerts.ts または同じ受付API一覧を読む。質問への回答・通常の作業・報告の前には読まない。有事の操作条件は chat-instructions.md を参照する。有事の期限判定と期限切れの対処は受付（NAS）の周期が行う。チャットのAIは有事の記録（要対応の経過）を読んで続け、受付の対処を重複して実行しない。利用者の返答は受付の要対応の「自分で対応する」で受け付ける。利用者専用の操作・一時キーの強制終了・明示された停止の解除・権限の追加は時間切れでも行わない。

相談への回答（`create-issue.ts --draft`）は送信前に、`skills/issue-desk-operations/SKILL.md` の「送信前のIssue番号確認」の手順で本文を照合して書き直す。

```
~/.issue-desk/desk desk-alerts.ts
~/.issue-desk/desk desk-alerts.ts --json
node scripts/desk-alerts.ts --url <受付のURL> --token-file <トークンのファイル>
```

- 受付画面の「要対応」と同じ全件数の要約と、依頼・相談ごとのまとめを出す。「止まっている」の基準は `--stale-minutes <正の整数>`（既定30分）。条件・元の経路・消える条件・処理の振り分けは `skills/desk-attention/SKILL.md` を参照する。「複数の依頼に広がる詰まり」の見分け方・直し方も desk-attention にある。`--json` は機械向け（items・monitoring・failures・thisHost・summary・cases）。itemsは利用者の判断・原因別の引継ぎ、monitoringは見張りが扱う失敗・止まり等の記録。読むだけ。
- 読めなかった経路は理由が出て、ほかの経路は表示が続く（終了コード1）。読取失敗を「要対応なし」と扱わず、取得できた経路の確認を続け、未確認の経路と理由を残す。403の経路は、そのトークンに運用の代行が許されていない。
- PCの状態（版・heartbeat・hostd周期結果）は `GET /api/hosts` で先に読む。止まりの原因は desk-alerts の `--json` の `monitoring[].detail`（JSONの止まり診断）から読む。
  ```sh
  sh ~/.issue-desk/desk-pc get --path /api/hosts
  ```
  `~/.issue-desk/desk` はNASとUbuntuのPCの窓口。このGETはホストの状態だけを読み、通常のPC別トークンで読める（運用の代行の許可は要らない）。`desk-alerts.ts` は運用の代行を許したトークンが要る。読取失敗（終了コード1）・不正応答・理由付きの空一覧・古いheartbeatを「正常」「待ちなし」と断定せず、取得時刻と欠測を残す。401・403を迂回しない。
- どのPCがどのリポジトリの依頼を受け取るかは、`assignments.json` の `hosts.<PC>.repositories` だけで判断する。受付はここから `implementers`（リポジトリごとの担当PCの順）を作って配る（`server/internal/fleet/assignments.go`）。`fleet.json` の `hosts.<PC>.repositories` はPCに配る設定（clone・テストのコマンド）で、配布先の根拠にしない。`GET /api/hosts` の ready・busy も、そのPCが対象のリポジトリを受け取るかを示さない。依頼が進まない理由に「空いているPCがある」と書く前に、そのPCが `assignments.json` で対象のリポジトリを持つかを照合する。
- 回答待ちの相談は、答えられるものを `create-issue.ts --draft <下書きID> --text "<回答>"` で答え、利用者の判断が要るものだけ利用者に回す。確認済みにする・閉じるかの判断は、下の「要対応の確認済み・クローズ依頼の判断・相談の取り下げ・TODO」の条件に当たるときだけ [手動運用手順](refs/manual-operations.md) で行う。Issueの停止・再開は 汎用 `operation`（`skills/issue-desk-issue-control/SKILL.md` の「Issueの優先度・停止・再開・担当を変える」の条件）で行う。条件に当たらなければ利用者に伝える。

## 要対応の確認済み・クローズ依頼の判断・相談の取り下げ・TODO

利用者の判断を受けて送る操作は、正本 [chat-instructions.md](../../chat-instructions.md) の箇条 (b)「根拠が変わったとき」を参照する。

要対応の確認済み、相談の終了・破棄・TODOへの退避、TODO削除、hostdの指示、拒否された後続案の投稿し直しは [手動運用手順](refs/manual-operations.md) に従う。本文・状態・指示日時・revision等を照合し、利用者の指示か記録と決まりで対処できるときだけ送る。結果不明を成功とせず、403を別認証へ切り替えて回避しない。

クローズ依頼の一覧・確認待ちのclose/keepと終了理由の表示変更は [Issueの終了手順](../issue-desk-issue-control/refs/close-issue.md) に従う。閉じてよい条件を確認できなければcloseもkeepも送らない。`closing` は未終了であり、Workerの結果を確認する。

保存済みTODOの本文更新と相談開始は [TODO手順](../issue-desk-operations/refs/todo-edit.md) を読む。代理回答取消は利用者がチャットで承認したときだけ issue-desk-issue-control Skillの承認手順で送る。

配布・起動後点検の `desk-ops.ts notice` は機械通知の入口として残る。要対応の確認済みや手動の終了操作には使わない。

## 実行先を一時的に無効にする（利用者の指示があったときだけ）

「Codex が切れたから Claude にして」などの利用者の指示による設定・取消と、状態の確認は [provider-disable](../provider-disable/SKILL.md) に従う。入力・窓口の例・応答の照合・断られたときの扱いは同Skillにある。利用者の指示が無ければ設定・取消をしない。

## 工程の固定規則とモデルを確認する

固定規則の確認と旧保存値の取消は [model-override](../model-override/SKILL.md) に従う。利用者が対象の確認・取消を求めたときに使う。

## PCの新しい引き取りを止める・戻す（再起動の前）

PC単位の停止・再開・状態確認と再起動前後の条件は [host-drain](../host-drain/SKILL.md) に従う。Issue単位の操作とは区別する。

## 未採用の後続案を対応不要として整理する

元Issueを閉じても残る `rejected`・`scan_excluded` の後続案は、利用者の指示、または記録から対処不要と説明できるときだけ [desk-attentionの後続案整理手順](../desk-attention/refs/proposal-dismissal.md) で整理する。元Issue・コメントを削除せず理由を保存する。採用済み・起票中の案は対象外で、既存の相談の終了手順を使う。

接続・本文指紋の照合・理由付き破棄・同一ID再送は desk-attention Skillの後続案整理手順に従う。指示より新しい本文や採用との競合は拒否されるので回避して送らない。詳細は [後続案の設計](../../docs/followup-proposals.md) を参照する。

- 利用者が対象の終了を明示したときだけ、NASでは `sh ~/.issue-desk/desk-pc operation`、Ubuntuでは `~/.issue-desk/desk operation` でPOST `/api/desk-cleanup/inspect` に対象を送って照合する。終了はPOST `/api/desk-cleanup/close` に照合で返った同じ `target` と `fingerprint`、理由 `reason`、利用者の指示原文 `userStatement` を送る。運用代行トークンが必要で、一時キーは使えない。既存の通知や状態を自動で消さず、原因修正・再開と区別する。対象形式、照合例、409と終了記録はdocs/chat-operations.md「明示終了API」。
