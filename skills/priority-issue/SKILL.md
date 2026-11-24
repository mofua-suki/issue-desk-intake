---
name: priority-issue
description: 受付のIssueの優先度を、利用者の指示または既存の割り込み条件に従って変更し、送信と現在値を確認する。
---

# Issueの優先度を変える

利用者が対象と変更を指示した場合、または [issue-desk-operations](../issue-desk-operations/SKILL.md)「起票する」のエージェントによる割り込み条件に当てはまる場合だけ行う。指示の順序・条件を広げない。実行待ち（`status` が `queued` / `dependency_waiting`）の間だけ変更でき、実際の可否は対象の状態と受付の拒否理由で確認する。Giteaへ直接コメントしない。

接続・対象照合・送信IDとJSONの保存・再送・失敗処理は [汎用窓口の共通手順](../issue-desk-issue-control/refs/api-operations.md#接続送信結果確認) に従う。NASは `sh ~/.issue-desk/desk-pc get` / `operation`、Ubuntuは `~/.issue-desk/desk get` / `operation` を使い、既存の運用代行PCトークンで送る。資格情報を表示せず、拒否時に一時キー・画面セッション・別アカウントへ切り替えない。

1. GET `/api/issue?desk=<共通番号>&view=summary` で返る `body` のproject・数値number・空でないtitle・`state`・`status` を確認し、対象と一致し実行待ちであることを確かめる（優先度の変更に本文・会話は要らないので要約で足りる）。`actions` はPC別トークンの応答には付かないので、その欠測を理由に送信を止めない。照合する欄の欠測・不一致なら送らない。
2. 初回送信前に小文字UUIDを生成し、対象・接続先・送り手の識別情報・JSON本文をホーム下の別cloneの `.tmp/` に保存する。`number` はGitea番号で共通番号ではない。
3. POST `/api/comment` に次のJSONを送る。`value` は `high` / `normal` / `auto`。`auto` は自動判定へ戻す明示値。`high` だけ `reason` が必須で、前後の空白を除きUnicodeコードポイントで1〜500文字（😀は1文字）。normal/autoではreasonを除く。停止・再開の `text` は付けない。

```json
{"id":"12345678-1234-1234-1234-123456789abc","project":"owner/repo","number":12,"action":"priority","value":"high","reason":"割り込みの理由"}
```

例の値を実対象に置き換え、異なる変更にはそれぞれ新しいUUIDを使う。保存したファイルを送るNASの例:

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/comment --body-file <ホーム下の作業ツリー>/.tmp/priority.json
```

4. POST成功は受付への送信であり現在値確認とは別。応答の `body.job` を確認する（応答のコメント `id` はnullでもよい）。同じ対象を `view=summary` でGETし、`body.priority.level` と `body.priority.source` を確認する。POST後のGET失敗や欠測は「送信済み・現在値未確認」と報告し、POSTせずGETだけを再実行する。

POSTの応答が不明なら新しいIDを作らず、保存した同じ接続先・送り手・対象・本文を同じ `operation` で受付に照合させる。拒否理由と `retryable` / `safeToChange` を読み、状態・権限を変えて回避しない。窓口の終了コード0だけで優先度確認済みと扱わない。停止・再開・担当変更は [issue-desk-issue-control](../issue-desk-issue-control/SKILL.md) に従う。
