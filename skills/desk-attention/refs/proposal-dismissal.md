# 未採用・走査除外の後続案を整理する

汎用窓口の接続・`body`・`redacted`・終了コードは [共通API手順](../../issue-desk-api/SKILL.md) の「汎用窓口の応答」に従う。

元Issueを閉じても残る `rejected`・`scan_excluded` は、利用者の指示、または記録から対処不要と説明できる場合だけ理由付きで終了する。元Issue・コメント・相談は削除しない。採用済み・起票中は対象外。復活・archiveの操作はない。

[共通接続](../../issue-desk-operations/refs/connection.md) に従う。運用代行を許したPCトークンを使う。NASの例は次のとおりで、Ubuntuでは `sh ~/.issue-desk/desk-pc` を `~/.issue-desk/desk` に替える。プロジェクト・Issue番号は事前に共通番号と照合し、queryの値をURLエンコードする。

```sh
sh ~/.issue-desk/desk-pc get --path '/api/proposals/dismissal?project=owner%2Frepo&number=<Issue番号>&proposalId=<案ID>&commentId=<コメントID>'
```

`body` のproject/number/proposalId/commentIdを対象と照合し、body・title/purpose/acceptance・postedAt/updatedAt・fingerprintを読む。fingerprintKindにより案JSONと元本文SHA256を区別する。案IDなしの形式不正はproposalIdを省略または空文字にし、commentIdで指定する。保存時のrecordedReasonと現在のparseErrorは別の事実。情報が無い古い記録を推測で補わない。

操作を決めた根拠と指示日時を確認する。PCトークンでは `instructionAt` が必須で、元の指示の日時をオフセット付きISO 8601で送り、未来や元コメント更新より古い日時を使わない。会話にない日時を捏造しない。内容が指示後に変わった場合は、その指示を使わない。

送信前にUUIDを1つ生成し、bodyを作業ツリー内の一時JSONに保存する。同じIDは内容を変えず再送に使う。

```json
{"project":"owner/repo","number":1,"proposalId":"案IDまたは空文字","commentId":10,"id":"送信UUID","reason":"対処不要と判断できる具体的な理由","fingerprint":"取得した小文字64桁SHA256","instructionAt":"元の指示の日時"}
```

number/commentIdは正の安全な整数、案IDは200文字以内、理由は空白だけを除き1〜1000文字。fingerprintは自分で再計算せず、照合GETが返した現在値を使う。

```sh
sh ~/.issue-desk/desk-pc operation --method POST --path /api/proposals/dismiss --body-file <JSONの絶対パス>
```

応答 `body.operationId`・fingerprint・対象・status・reasonを照合し、元の要対応一覧を再読取する。終了済み記録の同ID再送は同じ記録を返し、既存記録がrestoredなら再終了しない。409は内容変更や採用競合なので読み直し、異なる指紋へ古い操作を適用しない。結果不明時は保存した同ID・同内容で照合し、新IDで重ねない。403を別認証で迂回しない。一時JSONは用後に削除する。
