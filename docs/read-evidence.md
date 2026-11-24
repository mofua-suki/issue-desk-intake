# 読取済み資料の引継ぎ

新規相談では `--read-evidence-file evidence.json` を指定できる。本文とは別に保存される。`--draft`、`--check`、`--submit`、`--check-only` とは併用できない。

```json
{"version":1,"documents":[{"id":"source1","project":"owner/repo","path":"README.md","revision":"0123456789abcdef0123456789abcdef01234567","start":1,"end":2,"text":"1行目\n2行目"}],"facts":[{"statement":"確認した事実","documents":["source1"]}]}
```

```sh
node scripts/create-issue.ts --project owner/repo --tier light --text "依頼本文" --read-evidence-file evidence.json
```

`revision` は小文字40桁の固定コミット、範囲は1始まりで両端を含む。本文には行番号接頭辞を含めず、改行はLFに正規化する。documentのIDは一意、projectは相談先と一致し、factのdocumentsは存在するIDを1件以上含める。documentsは最大16件、factsは最大64件、JSONエンコード全体は65536 bytes以下。これは起票側の事実報告であり、Serverが内容の正しさや利用者の承認を証明するものではない。

同じ送信IDで再送するときは本文・指定値とreadEvidenceを同じにする。内容を変える場合は新しい送信IDが必要。

相談側は許可された資料を固定版から取得し、revision、範囲、本文を照合する。一致した範囲とその出典を持つ事実を相談入力に渡し、本文の配信後に確認済みとして扱う。一致しない資料や取得できない資料は理由を残し、その資料に依存する事実を再利用しない。引継ぎ資料を範囲指定なしで確認済みに挙げるには、通常の全文読取で本文が配信されている必要がある。対象範囲外は通常の資料読取で確認できる。AGENTS.mdなどの初期必須資料は引継ぎがあっても取得する。
