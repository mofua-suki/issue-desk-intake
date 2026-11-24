# 公開ジョブの前提確認

非公開のジョブ登録 JSON は対象IDをキーとし、各値に固定キー `entry` と、[公開候補の準備](public-source-publish.md) の登録 JSON の絶対パス `registry` を指定する。定時の自動判定を使う対象には後述の `policy` も指定できる。準備登録には同じ対象IDが必要。登録と基点は運用担当が審査する。

```json
{
  "intake": {"entry": "public-source", "registry": "/private/publish-registry.json"},
  "worker": {"entry": "public-source", "registry": "/private/publish-registry.json"}
}
```

```sh
sh scripts/run-public-source-jobs.sh --dry-run --registry /private/public-jobs.json
```

対象ごとに登録、入力と公開先のローカル基点、Git、Dockerのローカル検査イメージ、入口を確認する。全対象の前提が確認できれば終了コード0、不足があれば対象ごとに表示して非0で終了する。この確認は生成・公開前検査・CIの成功や公開の承認を意味しない。登録の内容や子プロセスの出力は表示しない。固定キー `public-source` は準備と公開の両方に `scripts/publish-public-source.sh` を対応させる。新しい対象は既存キーを登録して追加できる。新しい入口の追加には公開前検査とCI合格、および既存の個別承認と停止条件を維持する審査が必要。試運転は入口を実行せず送信しない。

## 候補準備

```sh
sh scripts/run-public-source-jobs.sh --prepare --registry /private/public-jobs.json --output /private/new-run
sh scripts/run-public-source-jobs.sh --prepare --registry /private/public-jobs.json --output /private/next-run --previous-dir /private/previous-run
```

出力先は存在しない絶対パスとし、直上の親には実行ユーザーが所有する非公開の実ディレクトリを用意する。出力先と候補木のルートは0700、証拠は0600で保存し、権限を設定できない場合は失敗する。対象ごとの `<対象>.tree` に検査済み木、`<対象>.json` に準備の証拠を保存する。結果は `prepared`、`no-diff`、`skipped`、前提不足、失敗を対象別に表示し、失敗があっても残りを処理する。前提不足または失敗があれば終了コードは非0。登録や子プロセスの出力は表示しない。準備は送信しない。

見送りに使う場合、前回のディレクトリに実際のマージを確認した `<対象>.merged.json` と、その時の `<対象>.tree` を対で保存する。ディレクトリ・記録・木のルートは実行ユーザーの所有とし、他ユーザーがアクセスできないモードにする。両方を安全に参照できない場合は見送りを使わず再生成する。マージ済み記録は準備では変更しない。候補と証拠を保管し、送信の前に内容の確認と必要な個別承認を行う。

## 一対象の送信

準備済みディレクトリと同じジョブ登録を使う。管理者は既存の送信プレビューで候補を確認し、個別承認を得た確認値を所有者のみ読める実ファイルに保存する。ジョブ登録、公開登録、準備ディレクトリ、候補、確認値は実行ユーザーの所有とし、他ユーザーにアクセスを許さない。シンボリックリンクは使えない。

```sh
sh scripts/run-public-source-jobs.sh --send --registry /private/public-jobs.json --target intake --prepared-dir /private/new-run --confirmation /private/confirmation < /private/issued.json
```

発行元から得た一対象分のJSONを標準入力に渡す。送信前に同じ固定入口で再プレビューし、確認値が一致した場合だけ送信する。結果は `published`、`merged`、`waiting` を表示し、終了コードはそれぞれ成功0、待ち2、失敗1。待ちや送信結果が不明な場合は公開先の状態を確認し、同じ候補の再実行を既存送信入口に委ねる。

## 承認済み対象の一巡

手動確認する対象の確認値を所有者のみ読める通常ファイル `/private/confirmations/<対象>` に保存し、ディレクトリも所有者だけが使えるようにする。発行登録は[発行手順](public-source-publish.md)の登録を使う。手動確認では対象ごとにプレビューを確認し個別承認を得てから実行する。

定時登録で個別に自動判定を許可する場合は、ジョブの値に `policy` として所有者のみ読める通常ファイルの絶対パスを追加する。省略時は手動確認値が必要。方針には次の全項目を指定し、登録内容と公開範囲を運用担当が審査する。方針ファイルはリンクやハードリンクを使えない。

```json
{
  "target": "intake",
  "repository": "example/project",
  "base": "refs/heads/main",
  "publicRepo": "/private/public-repo",
  "publicRef": "refs/heads/main",
  "sourceRepo": "/private/source",
  "sourceRef": "refs/heads/main",
  "preparedRoot": "/private/runs",
  "configSha256": "<準備証拠のconfigSha256>",
  "generatorSha256": "<準備証拠のgeneratorSha256>",
  "ciSha256": "<準備証拠のciSha256>",
  "ci": "intake",
  "verify": "intake",
  "paths": ["README.md"]
}
```

`preparedRoot` は一巡に渡す準備ディレクトリの直上の絶対パス。`base` と `publicRef` はともに `refs/heads/main` を指定する。3つのハッシュは審査した登録・生成器・CI定義の値を固定し、変更時は再審査する。通常方針の `paths` は公開登録の収録パスにも含まれる個別ファイルで、既存の通常テキストの内容変更だけを許す。追加・削除・実行属性変更・リンク・ワークフローや承認設定の変更は通常方針で拒否する。

追加ファイルを含む初回候補を自動判定に渡す場合は、候補の差分全体を審査し、任意の `initialCandidate` を方針の直下に追加する。値は審査した準備証拠の `publicBase`（公開基点）、`inputSha`（入力コミット）、`generatedTree`（出力 Git 木）だけを持つ JSON オブジェクトで、各値には小文字16進数の完全な Git OID を指定する。現行の両基点、候補コミットの木と唯一の親、準備証拠、方針の3つのハッシュと `ci`・`verify` がすべて一致する候補に限り、通常の個別パス判定を省略する。再生成時のコミット日時の差は許容する。3つの OID が一致しない候補や、マージ後に進んだ公開基点の候補には通常方針を適用する。

発行前のローカル検査で外部取得を起こさないよう、入力と公開先に部分クローンは使えない。初回候補にも、生成器の禁止項目検査と隔離検査、送信時の保護と個別承認が適用される。自動判定では各回のプレビュー確認値を一時ファイルに入れ、送信処理終了時に削除する。

```sh
node scripts/run-approved-public-source-jobs.cjs --registry /private/public-jobs.json --prepared-dir /private/new-run --confirmations-dir /private/confirmations --issuer-registry /private/app-registry.json
```

準備結果が `no-diff` または `skipped` の対象は証拠を検査して見送り、発行・送信・確認値の読取を行わない。候補がある対象は発行前に再生成・プレビューし、手動では保存した確認値と照合、自動では方針に照合してから発行する。一対象送信入口でも送信直前に再プレビューして照合する。結果は対象ごとの `no-diff`、`skipped`、`published`、`merged`、`waiting`、`failed`。見送りではマージ済み記録を更新しない。失敗があっても残りを処理し、終了コードは失敗があれば1、待ちのみなら2、全対象成功なら0。待ちや結果不明の場合は公開先を確認し、同じ候補と確認値で再実行する。候補と承認を再作成しない。発行登録、ジョブ登録、公開登録、候補、確認値には所有者以外がアクセスできず、リンクを使えない。
