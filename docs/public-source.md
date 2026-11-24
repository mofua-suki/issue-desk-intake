# 公開ソースの生成

Git、Node.js 24 以上、GNU coreutils の `mv` を使う。ソースの入力はローカル Git リポジトリのコミット。入力リポジトリの未コミット変更と Git 履歴は含まれない。GNU coreutils の `mv` を実行時に確認し、それ以外では出力を確定せず停止する。出力ルートのモードは 0755。出力先は存在しないパスを指定する。生成器は公開や push を行わない。`scripts/` と `deploy/public/` を含むチェックアウトから実行する。

標準定義 `scripts/public-source-policy.json` は両対象とも収録パスが空で、公開可否を確認していないファイルの生成を停止する。対象コミットごとに公開するパスと置換を確認してローカル定義を作る。ディレクトリやワイルドカードではなく、コミット内の通常ファイルの相対パスを一件ずつ `paths` に追加する。新しいソースファイルは収録定義への追加が必要。シンボリックリンク、Git サブモジュール、秘密鍵などの禁止パスは収録できない。Worker方式（登録方式では CI と検査の両方が worker）に限り、`internal/runtime/` 直下の Go ファイルと、その `testdata/` 内で公開 Go テストが参照する公開可能な合成 JSON を個別登録できる。各ファイルの公開可否を確認し、実行データや資格情報を登録しない。これらも通常ファイル判定と置換後の内容検査を通る。

intake・worker の生成物のルートには `LICENSE` が入る。`paths` で入力コミットの `LICENSE` を指定し、公開先も `LICENSE` の場合は、そのファイルを使う。それ以外は生成器を実行したチェックアウトにある `deploy/public/LICENSE`（Apache License 2.0）を使う。どちらも公開前検査の対象になる。

## 登録方式による生成と検査

登録定義の `outputs` に、`paths` に登録済みの元パスをキー、公開先の相対パスを値とする対応を任意で指定できる（例: `"outputs": {"src/tool.sh": "tools/public.sh"}`）。省略した元パスは同名で出力する。元と公開先はいずれも禁止パスの検査を受け、公開先の重複・祖先子孫関係や生成する CI・LICENSE との衝突は拒否される。置換は元パスで指定し、内容検査後の公開名とモードを候補木の照合に使う。


後続の公開処理には登録方式の結果を使う。`scripts/public-source-policy.json` の `registry-example` は架空値の書式例であり、そのままでは生成できない。対象IDごとに `repo`（絶対パス）、`sha`（固定コミットの完全な小文字SHA）、`paths`、`replacements`、`reject`、`ci`、`verify`、`image` を指定したJSONを非公開の場所に置く。`ci` と `verify` はそれぞれ `intake` または `worker`。第3対象も同じ方式を選んで登録できる。設定にコマンドは書けない。対象、固定SHA、収録範囲、置換、禁止値、CI、検査方式、イメージを変更する前に内容を審査し、承認済みの設定だけを使う。

`image` は事前準備した検査イメージの `名前@sha256:ダイジェスト`。Dockerと、選択方式のGoツールチェーン（`/usr/local/go/bin`）、固定依存を `/opt/public-deps/go` に備えた資格情報を含まないイメージが必要。intake方式はさらにNode.js、pnpm（`/opt/agent-tools/bin` または `/usr/local/bin`）と固定依存のオフラインストア `/opt/public-deps/pnpm` を必要とする。イメージにボリュームを定義しない。入力SHAの要求するGo版と依存を照合する。イメージの取得は生成器では行わない。コンテナはネットワークなし、非root、読取専用、全capabilityを削除して起動し、候補木は読取専用で渡す。作業領域はコンテナ内の一時ファイルシステムだけを使い、Dockerソケットは渡さない。隔離できない場合や依存不足の場合は失敗する。

```sh
sh scripts/build-public-source.sh --target intake --registry /private/public-registry.json --output /tmp/intake-public
sh scripts/build-public-source.sh --target worker --registry /private/public-registry.json --output /tmp/worker-public
```

成功すると `input-sha`、登録ファイルの `config-sha256`、確定する木の `tree-sha256` と成功を表示する。木の識別値は相対パス、ファイル種別、モード、内容から計算する。生成木と同じ作業用コピーでintakeではクライアントをオフラインでビルドし、対象のGoバイナリをビルドする。intakeではヘルプのusageを、workerでは `version` の成功と `issue-desk-worker ` で始まる出力を確認し、共通して不正引数の拒否とGoテストを実行する。検査中に生成木が変わった場合を含め、いずれかの失敗では成果物を確定せず成功記録も出さない。子プロセスの生出力は表示しない。既知の秘密鍵・トークン形式と登録した禁止値も全出力で検査するが、すべての秘密情報の検出は保証しない。公開前には収録内容の目視確認が必要。

## 従来形式の定義更新と生成

以下は `README.md` を確認済みの場合の定義更新例。各対象には `paths`、`replacements`、`reject` の配列が必要。置換は対象パスと置換前後の文字列を明示し、置換元が存在しなければ停止する。`reject` は出力の各ファイルに含まれてはならない文字列の一覧。実際の環境固有値や資格情報はリポジトリに書かず、安全に管理したローカル定義に設定する。

```sh
cp scripts/public-source-policy.json /tmp/public-policy.json
# /tmp/public-policy.json の対象を確認して編集する例
node - <<'NODE'
const fs = require('fs');
const file = '/tmp/public-policy.json';
const policy = JSON.parse(fs.readFileSync(file, 'utf8'));
policy.intake.paths = ['README.md'];
policy.intake.replacements = [];
policy.intake.reject = ['INTERNAL_HOST_TO_REJECT'];
fs.writeFileSync(file, JSON.stringify(policy, null, 2) + '\n');
NODE
sh scripts/build-public-source.sh --target intake --repo /path/to/intake --ref COMMIT_SHA --output /tmp/intake-public --policy /tmp/public-policy.json
sh scripts/build-public-source.sh --target worker --repo /path/to/worker --ref COMMIT_SHA --output /tmp/worker-public --policy /path/to/worker-policy.json
```

Worker も同じ形式で `worker` の配列を編集する。定義に新しいパスを加える前に内容と公開可否を確認する。出力先が既存の場合、ref が不正な場合、指定ファイルや置換元が欠ける場合、検査に失敗した場合は停止する。

`host-ssh.json` と `deploy/nas-host/install-github-actions-runner.sh` の両方を収録するときは、置換後の `operations.install-github-actions-runner` 内の唯一の SHA-256 と、置換後のスクリプトの SHA-256 を出力確定前に照合する。ハッシュは自動更新されない。不一致で停止した場合は、対象コミットの固定操作とスクリプト、およびローカル定義の両ファイルへの置換を確認する。公開するスクリプトに置換を適用した内容の SHA-256 を計算し、固定操作に記載する値と一致するよう必要な定義を修正する。固定操作にハッシュが一つだけ含まれることを確認し、存在しない新しい出力先を指定して再生成する。

## 一覧、検査、差分確認

従来形式では収録ファイルと検査結果が標準出力に表示される。どちらの方式でも手元で次のように一覧と内容を確認する。

```sh
find /tmp/intake-public -type f -print | LC_ALL=C sort
rg -n 'INTERNAL_HOST_TO_REJECT' /tmp/intake-public || true
diff -ru /path/to/reference-public-tree /tmp/intake-public
```

`diff` は差分があると終了コード 1 を返す。機械検査は既知の禁止文字列と秘密鍵の一部を検出するだけであり、資格情報、環境固有設定、実行データやその他の非公開情報が含まれない保証ではない。各ファイルと差分を目視で点検し、問題があればローカル定義を修正して新しい出力先に再生成する。置換文字列や検査文字列も公開対象に残らないよう管理する。
