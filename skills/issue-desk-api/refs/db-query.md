# 許可付きのDB参照

汎用窓口の接続・`body`・`redacted`・終了コードは [共通API手順](../SKILL.md) の「汎用窓口の応答」に従う。

受付のDB読取は `POST /api/diagnostics/db-query` を使う。画面セッションは対象外。運用代行を許したPCトークンと、対象 `shared`（共有DB）または `app`（アプリDB）に有効な期限付き `db-read` 許可が必要。[許可の付与](../../issue-desk-issue-control/SKILL.md) に従い、この会話の対象・期間が明示された発言で登録し、用後に取り消す。APIがあることを読取許可と扱わない。資格情報をログに出さない。

SQLは空でないSELECTまたはWITHから始まる1文、UTF-8で65,536 bytes以下。ファイルの先頭BOMは除く。SQLには資格情報や秘密の実値を埋め込まない（SQL全文は監査対象）。maxRowsは整数1〜1000、既定100。受付はREAD ONLY・検索パス・禁止対象/関数・5秒のstatement timeout・500msのlock timeoutを適用し、対象を問わず同時実行1件に制限する。

対象・SQL・maxRowsを作業ツリー内の一時JSONへ保存する。例:

```json
{"target":"shared","sql":"SELECT status, count(*) AS n FROM jobs GROUP BY status","maxRows":100}
```

NASの既存PCトークン窓口（Ubuntuでは `~/.issue-desk/desk operation`）で送る。

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/diagnostics/db-query --body-file <JSONの絶対パス>
```

HTTP成功だけで終えず、応答 `body` が次の形であることを確認する。不正応答を空結果・成功として報告しない。汎用窓口の `redacted: true` は秘密に相当する値を秘匿した印なので、表示値をDBの生値として比較・集計しない。

- columnsは空でない配列、各要素に文字列name/type。
- rowsは配列で、各行も配列かつcolumnsと同じ列数。空配列は0行。SQL NULLはnullのまま保持し、列順・重複列名を保持する。
- truncated.rowsとtruncated.bytesはboolean。最大1000行または256 KiBの打切りは不完全な結果であり、全件としない。

403は運用代行の拒否・db-read付与不足・認証区分の違いを受付の理由で区別し、別経路へ切り替えない。400/409/504、不正応答、未受理印のない503・切断では送り直さず未確認/失敗を報告する。受付が処理前の未受理印 `x-issue-desk-not-accepted` を返した503と確認できる場合だけ、同じSQLを短い待ちの後に規定回数内で再送できる。汎用窓口が印を表示しない場合は未受理と推定しない。送信IDによる照合はない。用後に一時JSONを削除しdb-read許可を取り消す。
