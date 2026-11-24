---
name: model-override
description: 利用者が工程の固定モデル規則の確認、または旧モデル切替の保存値の取消を求めたときに使う。
---

<!-- skill-standard: 1 -->

# 工程の固定規則と旧切替の取消

固定規則の確認と、利用者が対象工程の取消を指示した場合の旧保存値の削除を扱う。その他の運用管理は [issue-desk-admin](../issue-desk-admin/SKILL.md)、接続は [共通接続](../issue-desk-operations/refs/connection.md) と [受付APIの認証](../issue-desk-api/SKILL.md) に従う。実行先の一時無効化は別の操作である。

受付は工程規則を固定し、新規切替は409で拒否する。保存済み切替も実行に適用しない。相談・起票・クローズ・優先度は相談元、実装は起票時の実装先（省略時Codex）、修正は実装先、レビューは実装の反対側を使う。実行先の無効指定は別途考慮される。

一時キー、または利用者が受付画面で運用の代行を許したPC別トークンを使う。画面の場合はセッションを使い、変更には同一OriginとCSRFが必要。認証失敗時に別方式へ切り替えたり権限を変更したりしない。トークンや認証ヘッダーを出力しない。窓口や操作の実行許可がない場合は回避せず利用者に伝える。

## 一覧と取消

既存の汎用窓口を使う。NASのPCトークン窓口で一覧を読む例（Ubuntuでは `~/.issue-desk/desk get`）:

```sh
sh ~/.issue-desk/desk-pc get --path /api/model-overrides
```

応答 `body` の `locked`、`policy`、`defaults`、`steps` と `problem` を確認する。`defaults.intake.tiers` は段別、`defaults.review.opposite` は実装先ごとのレビュー設定。欠測や `problem` があれば該当部分は未確認とする。固定版の `overrides` は常に空で、旧保存値の有無を示さない。空一覧を旧保存値が無い証拠にしない。

利用者が指定した工程が `intake`・`review`・`priority` のいずれかであることを確認し、その工程だけ取り消す。以下は `intake` の例。NASでは運用代行トークンの窓口、Ubuntuでは通常の窓口を使う。

```sh
printf '%s' '{}' | sh ~/.issue-desk/desk-pc operation --method POST --path /api/model-overrides/intake/cancel --body-file -
```

応答を確認できる窓口では `cancelled: true` は保存値を削除、`false` は対象の保存値がなかったことを示す。値の欠測は未確認。汎用窓口の応答 `body.cancelled` を確認し、終了コード0だけで値を推定しない。固定規則は一覧で再確認できるが、その空の `overrides` から削除の成否は確認できない。応答を確認できなければ取消結果は未確認と報告する。

403は拒否として伝え、認証区分と既存トークンの運用代行許可を確認する。別経路へ回り込まない。409、その他のエラー、不正応答、通信失敗も成功と扱わない。通信断などで結果不明なら無条件に再送せず未確認と伝える。この取消APIには送信IDがなく、一覧では削除の成否を照合できない。別の送信IDによる照合がある操作では送信前にIDを保存し、結果不明時は同じIDで照合する。
