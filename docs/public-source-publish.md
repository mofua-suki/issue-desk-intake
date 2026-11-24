# 公開候補の準備

`scripts/publish-public-source.sh` は信頼済みのローカル Git を使い、1対象の検査済み公開木を準備する。Git、Node.js 24以上、生成器の検査環境が必要。生成器の隔離条件と公開前の目視確認は [公開ソースの生成](public-source.md) に従う。公開先の取得と基点の信頼性確認は実行前に行う。準備処理はネットワークへの送信、push、PR作成を行わない。

非公開の登録 JSON は対象IDをキーとし、各値に `source`、`ref`、`publicRepo`、`publicRef` を設定する。`source` は生成器の登録定義から `sha` を除いたもの、`ref` は入力リポジトリの信頼済み基点、`publicRepo` は公開先のローカル Git リポジトリの絶対パス、`publicRef` は照合する公開先の基点である。追加対象も同じ書式で登録する。登録と両基点は運用担当が審査する。任意の `author` に `name` と GitHub の `users.noreply.github.com` メールを指定すると候補の著者に使う。両方を省略する場合は `author` 自体を省略し、著者は Public Source となる。コミッターは常に Public Source である。名前は空白のみ、前後の空白、制御文字、山括弧を含められない。メールはユーザー名または `ID+ユーザー名` 形式に限る。

```json
{
  "example": {
    "source": {
      "repo": "/private/source",
      "paths": ["README.md"],
      "replacements": [],
      "reject": [],
      "ci": "intake",
      "verify": "intake",
      "image": "example.invalid/check@sha256:0000000000000000000000000000000000000000000000000000000000000000"
    },
    "ref": "refs/heads/main",
    "publicRepo": "/private/public-repo",
    "publicRef": "refs/heads/main"
  }
}
```

```sh
sh scripts/publish-public-source.sh --target example --registry /private/publish-registry.json --output /private/candidate > /private/prepared.json
```

出力は JSON で、`status` が `prepared` なら生成木と公開先に差分がある。`no-diff` は生成・検査した木が公開先と同じことを示す。`skipped` は前回マージ済みの記録と対象・入力SHA・公開設定・生成器版・CI定義版が一致し、手動指定の場合は保存された候補木の識別値も一致したことを示す。`prepared` と `no-diff` では指定先に検査済みの木を残す。これらはマージ済み記録を更新しない。

準備時に候補の全ファイルのパス・内容・実行属性を一時インデックスと照合し、不一致なら失敗する。`prepared` の JSON は送信用ローカルコミットの OID を `commit`、その唯一の親となる固定公開基点の OID を `publicBase` に含む。コミットの木は `generatedTree` と一致する。`no-diff` と `skipped` には `commit` と `publicBase` を含めない。ローカル参照は更新しない。

省略を使う場合は、実際のマージを別途確認した後、その候補の JSON の `status` を `merged` とした信頼済み記録と、その時の候補木を安全に保存する。次回は次のように両方を指定する。

```sh
sh scripts/publish-public-source.sh --target example --registry /private/publish-registry.json --output /private/new-candidate --previous /private/merged.json --previous-tree /private/merged-tree
```

`send.mergeMethod` が `merge` の登録では、マージを公開先で照合した後に `stateDir/<対象>.merged.json` を原子的に更新する。次回の準備はこの記録のリポジトリと証拠を照合し、マージコミットが現在のローカル公開基点から到達可能で、その木と二つの親が記録と一致する場合に自動で見送る。記録は実行ユーザー所有のモード0600の通常ファイルとし、リンクや正規化されていない状態ディレクトリのパスは使えない。記録がない場合や証拠が一致しない場合は生成する。手動指定にも候補木を保存できる。公開先と同じ木の場合は `stateDir/<対象>.no-diff.json` に別途記録し、マージ済み記録は更新しない。公開先のローカル参照は準備前に取得・照合する。失敗時は原因を確認し、出力先が残っていれば内容を確認してから別の存在しない出力先で再実行する。候補木や登録には非公開情報が含まれる可能性があるため非公開の場所で管理する。

## 送信前の資格情報照合

`scripts/public-source-credentials.cjs` の `canPublish` は送信可否の真偽値だけを返す。引数は `registration`（信頼済み登録の `repository` と `credentialRef`）、`candidate`（要求する同名の値と `changesWorkflow` 真偽値）、`credential`（信頼済み発行結果の同名の値、`permissions`、任意の `expiresAt`）である。`repository` は完全な `owner/repo`、`credentialRef` は英数字で始まる英数字・`.`・`_`・`:`・`-` からなる128文字以内の非秘密識別子とし、値は完全一致で照合する。`expiresAt` はUTCのISO日時で、指定があれば現在時刻より後でなければならない。時刻を指定する場合は `now` にUnixミリ秒を渡す。`contents` と `pull_requests` は `write` が必要で、workflow変更時は `workflows` も `write` が必要となる。欠測や不正な値は拒否する。

呼び出し側は発行元から直接得た対象・参照・権限・期限を実際に使う資格情報へ結び付け、信頼済み登録と候補を送信直前に照合する。候補の申告で発行結果を補わない。秘密トークンをこの関数に渡さず、判定後も既存の個別承認を確認する。この関数は送信、資格情報の取得、保存を行わない。

## GitHub App トークンの発行

Node.js 24以上を使う。実行ユーザー所有で他者からアクセスできない通常ファイルに、対象名をキーとした登録JSONとRSA秘密鍵を保存する。リンクは使えない。登録には `id`（GitHubの整数リポジトリID）、`full_name`（`mofua-suki/issue-desk-intake` または `mofua-suki/issue-desk-worker`）、`appId`、`installationId`、送信登録と一致する `credentialRef`、秘密鍵のパス `keyPath` を指定する。

```json
{"example":{"id":123,"full_name":"mofua-suki/issue-desk-intake","appId":"42","installationId":"43","credentialRef":"example","keyPath":"/private/app-key.pem"}}
```

下記の送信プレビューで候補と確認値を確認し、適用される個別承認を得てから、発行結果を直接送信の標準入力へ渡す。

```sh
node scripts/issue-public-source-github-app.cjs /private/app-registry.json example | sh scripts/publish-public-source.sh --send --target example --registry /private/publish-registry.json --candidate /private/prepared.json --tree /private/candidate --confirm <確認値>
```

発行はGitHub APIに接続し、対象・権限・期限を照合した場合だけJSONを出力する。失敗時は発行CLIが非0で終了し、標準出力は空になる。パイプの終了状態だけでは発行側の失敗を判定できないシェルでは、両コマンドの終了状態を確認する。

## 検査済み候補の送信

送信する対象の登録には `send` として `repository`（GitHub の `owner/repo`）、`base`（`refs/heads/` で始まる公開基点）、`credentialRef`、`stateDir`（運用者間で共有する非公開の絶対ディレクトリ）、通常のマージを行う場合は `mergeMethod: "merge"` を追加する。`stateDir` は永続的でディレクトリの同期と排他的な作成をサポートする場所に事前に作り、同じ公開先を操作する全プロセスで同じものを使う。登録と発行元の参照・権限を管理者が確認する。公開先は `github.com` に限定する。

準備の JSON と候補木を保存し、同じ登録で送信プレビューを行う。

```sh
sh scripts/publish-public-source.sh --send --target example --registry /private/publish-registry.json --candidate /private/prepared.json --tree /private/candidate
```

候補を再生成して全ファイルの内容とモード、Git木、コミットの唯一の親を照合する。表示された公開先・基点・候補と確認値を確認し、適用される個別承認を得る。送信時は同じ引数に `--confirm <確認値>` を付け、信頼済み発行元から直接得た JSON を標準入力に渡す。JSON は `token` と、同じ発行結果の非秘密情報 `metadata`（`repository`、`credentialRef`、`permissions`、任意の `expiresAt`）を持つ。資格情報は記録やコマンド引数に置かない。登録・候補・発行結果が一致し必要な権限がある場合だけ送信する。生成器には送信資格情報を渡さない。

候補枝は対象・公開基点・候補木から決まり、存在しない場合だけ作成する。同じ head の枝とPRは再利用し、異なる head や基点の変更では上書きせず停止する。枝作成後とPR照会・作成後にも基点と枝を確認する。`mergeMethod` のない登録では成功時に `published` を返す。`merge` を指定すると同じPR、厳格な枝保護と必須チェックの発行元・成功を確認し、head SHAを指定して通常マージを要求する。マージ済みPR、公開枝のマージコミット、その親と木を照合した場合だけ `merged` を返し記録する。送信時の空の期待値を持つ `--force-with-lease` は候補枝が存在しない場合に限る作成であり、既存枝を更新しない。

PR作成の直前に `stateDir` へ未確定記録を残す。応答が失われた場合は同じ候補と確認値で再実行し、一致するPRがあれば再利用する。照会できない場合やPRが見つからない場合は再作成せず停止する。運用担当は公開先のPRと候補枝を確認し、作成要求の結果が確定するまで記録を保持する。要求が処理中でなくPRが作成されなかったことを確認できた場合は、該当記録を解除して再実行できる。記録名は `sha256(JSON.stringify([repository.toLowerCase(), branch, base])) + '.pending'` で、`branch` はプレビューの値、`base` は登録の完全な基点refである。中断で `.lock` が残った場合は実行中の送信がないことを確認してから運用担当が解除する。基点が変わった場合は新しい候補を準備し、改めて内容を確認して個別承認を得る。

マージ結果が不明な場合は同じ登録・候補・確認値で再実行する。公開先に一致するマージ済みPRがあり、公開基点がそのマージコミットを指す場合に限り読み戻して記録する。マージ後に基点が進んだ場合は記録せず停止する。運用担当は公開先のPR、マージコミットの親と木、および後続の変更を確認する。公開先を巻き戻したり未確認のマージ済み記録を作ったりせず、現在の公開基点をローカルに取得・照合し、記録の照合結果を確認し、新しい出力先に候補を準備する。一致する過去のマージ記録があれば後続の公開変更があっても `skipped` となる。後続変更を含む候補を必要とする場合は、運用担当が記録と公開履歴を確認してから該当するマージ記録を安全に退避し、再度準備する。`no-diff` なら公開先と生成木が一致しており送信は不要。`prepared` なら後続の変更を含む差分を確認し、必要に応じて入力や公開設定を調整したうえで再生成し、改めて個別承認を得て送信する。準備と `no-diff` では以前のマージ済み記録は更新されず、新たなマージが照合された場合に更新される。

保護やチェックが不足・保留・不一致の場合は停止し、成功記録を進めない。通常マージでは公開基点に適用される rulesets と従来型 branch protection の必須チェックを照合する。従来型の `required_status_checks` が `Branch not protected` の404を返す場合は、厳格な必須チェックを含む rulesets 単独の構成でも実行できる。それ以外の取得失敗や曖昧な発行元は拒否する。有効化前に対象限定の資格情報、通常マージの許可、必須チェックと発行元、基点の競合を拒否する厳格な保護、pc-ciの稼働と信頼境界を確認する。未承認のコードがNASや公開資格情報のある環境で動く構成では有効化しない。GitHubのネイティブauto-mergeはこの方式に不要である。
