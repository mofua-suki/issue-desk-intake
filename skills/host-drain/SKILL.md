---
name: host-drain
description: 利用者がPCの新しい仕事の引き取りの停止・再開、またはその状態確認を求めたときに使う。再起動前の安全確認を含む。
---

<!-- skill-standard: 1 -->

# PCの引き取り停止・再開

対象PCと操作の依頼を確認する。Issue単位の停止・再開は [issue-desk-issue-control](../issue-desk-issue-control/SKILL.md)、Workerの起動・監視は [check-gitea-issues](../check-gitea-issues/SKILL.md) を使う。停止は既存の実装・公開を中断せず、新規引き取りだけを止める。OS再起動の許可を兼ねない。

[共通接続](../issue-desk-operations/refs/connection.md) に従い既存の汎用窓口を使う。以下はNASのPCトークン窓口。Ubuntuでは `sh ~/.issue-desk/desk-pc` を `~/.issue-desk/desk` に置き換える。手動停止・再開APIはPCトークンの運用代行許可を要しないが、利用者の操作依頼は必要。認証失敗時に別経路へ切り替えない。

## 対象と期限

まず一覧を読む。

```sh
sh ~/.issue-desk/desk-pc get --path /api/hosts
```

応答 `body.hosts` で対象の `name`、`updatedAt`、`alive`、`drain`、`restartSafe`、`implementing`、`publishing` を照合する。対象名は英小文字・数字で始まり、英小文字・数字・ハイフンの1〜31文字。理由付き空一覧、欠測、対象行なし、古いheartbeatは正常や0件としない。詳しい診断は [ホスト情報](../desk-attention/refs/hosts.md) を読む。

- 理由は空白を除いて1〜200文字。停止期限は省略時に受付の既定（未設定時150分）。停止中の再POSTは印を上書きする。
- `1h`・`3h` の依頼は現在から1・3時間後を計算し、`morning` は受付PCで次に来る7:00へ変換する。受付の時計・タイムゾーンが分からなければ推測して送らない。
- 日本時間の日時には `+09:00` を付け、実在する日時か照合する。2月30日等の自動繰越を許さない。送信値は未来かつ30日以内のオフセット付きISO日時。略記をそのままAPIへ送らない。
- 利用者が無期限停止を明示したときだけ `indefinite: true` を使い、`until` と同時指定しない。

## 送信と確認

停止bodyを作業ツリー内の一時JSONへ保存する。例は `{"reason":"保守のため","until":"<検証済みISO日時>"}`。既定期限なら `until` を省く。送信前に対象と期限を操作依頼に照合する。

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/hosts/<ホスト名>/drain --body-file <JSONの絶対パス>
sh ~/.issue-desk/desk-pc get --path /api/hosts
```

再開は同じ経路へ本文なしでDELETEし、一覧を読み直す。

```sh
sh ~/.issue-desk/desk-pc operation --method DELETE --path /api/hosts/<ホスト名>/drain
sh ~/.issue-desk/desk-pc get --path /api/hosts
```

POST/DELETEの応答はhostとdrainであり一覧ではない。成功応答と一覧の停止印・期限・理由、または再開後の印消去を照合する。操作成功後の一覧だけ読めなければ「操作成功・現状未確認」を区別する。送信IDのないAPIなので、通信断では一覧で結果を照合し、無条件に再送しない。403を迂回せず、409は状態を読み直す。不正応答や読取失敗を成功にしない。一時JSONは用後に消す。

`/api/v1/hosts/{host}/drain` はhostd更新用で、手動操作の代わりに使わない。停止中もhostd更新は進む。手動DELETEは保存された印全体を消すため、hostd更新中の印も確認する。

## 再起動前後

再起動直前に一覧を取得し、十分残った有効な停止期限、新しいheartbeat（`alive: true` と `updatedAt`）、`restartSafe: true`、`implementing: 0`、`publishing: 0` をすべて確認する。欠測・期限切れ・falseなら再起動しない。許可済みの時刻に再起動し、起動後は新しいheartbeatと監視・自己検査を確認してから再開する。
