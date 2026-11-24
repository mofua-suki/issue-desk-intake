# Macをテスト専用・実装兼用に分けて追加する

MacのネイティブテストはmacOS上で、実装担当WorkerはDocker DesktopのLinuxコンテナで動かす。同じMacでもPC別トークン・ホストID・状態ディレクトリを分ける。ネイティブテストだけならDocker・Codex・Claudeのログインは不要。

| 利用方法 | ネイティブテストID | 実装用ID |
| --- | --- | --- |
| テスト専用 | `<名前>-test` | 追加しない |
| 実装兼用 | `<名前>-test` | `<名前>-worker` |

テスト専用IDは実装の仕事・PR作成・マージ依頼を引き取れない。実装用IDは通常の担当順位・同時実装上限に従う。Mac検証を必須にしたリポジトリでは、マージの番の取得・更新に対応版の宣言とMac設定ダイジェストが必要で、旧Workerや旧設定によるマージを拒否する。解放は旧版でも可能。Apple Siliconでも実装用Workerは配布イメージに合う `linux/amd64` を使用し、Mac本体でXcodeの検証を行う。

## 登録内容を準備する

NASの配置先ではなく、Intakeの別cloneで実行する。`--home` は追加するMacの実際のホームに置き換える。

```sh
~/.local/bin/node scripts/mac-registration.ts --name macbook --role test-only --home /Users/利用者名 --out .tmp/macbook-registration
```

実装も担当させる場合は `--role implementation` を指定する。`--gate off` を付けると登録だけ行い、実装担当へ `nativeTestHost` を付けない（Mac検証を必須にせず、検証要求もMacへ届かない停止の状態）。対象が関連リポジトリのグループに属する場合は、Server/Clientの両方を実装担当に加える。Macネイティブ検証の必須条件はClientだけに付ける。名前は24文字以内。対象は既定で `mofua-suki/sample-expense-client`。出力先は `.tmp/` 配下の空フォルダーのみ。スクリプトは登録案の `fleet.json` と `assignments.json`、実装兼用では `deploy/nas/worker-release.json` のイメージ配布先も作り、本番・追跡ファイル・トークンは変更しない。

生成された差分を確認して専用ブランチへ反映し、`sh scripts/verify.sh` に成功したPRをマージする。`--gate off` を付けないときは、同じリポジトリの既存実装担当すべてに `nativeTestHost` が付く。その場合はMacの追加と同時にそのMacの検証がマージの必須条件になるため、Macの道具と接続を先に準備し、登録後は速やかにrunnerを起動する。

MacにはFlutter 3.47.9・Xcodeの初回設定・Git・Goが必要。生成した `nativeToolRoots` は `<Macのホーム>/development/flutter` を指すため、SDKを別の場所へ置く場合は登録案の値を合わせる。`nativeNetwork: true` は隔離したテストでの依存取得・アプリ通信・Flutterのローカル検証接続を有効にする。資格情報・runner制御状態へのアクセスは許可しない。精算Clientの `docs/mac-testing.md` に従い、先に `scripts/verify-apple.sh ios` が単体で成功することを確認する。Xcodeのライセンス同意やツールのインストールはこの仕組みが代行しない。

## 登録済みのMac

| ホストID | 機種 | 役割 | 対象 | 状態 | 初期セットアップ |
| --- | --- | --- | --- | --- | --- |
| `macbook-pro-test` | MacBook Pro | テスト専用 | `mofua-suki/sample-expense-client`、`mofua-suki/alarm-checklist` | 稼働中（両リポジトリの実装担当 gt12・nab-9s の `nativeTestHost`） | `deploy/clients/macbook-pro-setup.sh` |
| `macbook-air-test` | MacBook Air | テスト専用 | `mofua-suki/sample-expense-client` | 停止中 | `deploy/clients/macbook-air-setup.sh` |

どちらも `--gate off` で登録し、登録したホームはMacBook Proが `/Users/user`（Mac上で確認済み）、MacBook Airが `/Users/user`（推定。Mac上で未確認）。両方とも検証のコマンドは `sh scripts/verify-apple.sh ios`（Xcodeでのシミュレーター向けビルドと、シミュレーター上のテスト。起動済みのシミュレーターが無ければ精算Clientのスクリプトが1台起動する）。MacBook Proは稼働中で、Giteaのアカウント `issue-desk-worker-mac-book-pro` を `assignments.workerAccounts` に書いてある（Gitea上に作り、各検証対象を読めるようにしておく）。稼働中のMacBook Proは、両対象の全実装担当（gt12・nab-9s）の `nativeTestHost` で、実装担当のPCはマージの前にMacBook Proへ検証要求を送り、その成功をマージの条件にする。停止中のMac（MacBook Air）はどの実装担当の `nativeTestHost` にも無く、Mac検証はそのMacの条件にならず、検証要求も作られない。PCの手動停止印（`/api/hosts/<ID>/drain`）は受付にheartbeatの行が無いPCへ付けられず、Macのネイティブテストの引き取りも停止印を見ないため、停止はこの設定で表す。`--gate off` はGitea上のテスト専用アカウント（`assignments.workerAccounts`）も書かないので、稼働させるときに作って登録する。実装兼用の実装用IDは `--gate off` でも通常の担当として引き取るため、そのアカウントは書く。

初期セットアップはMacのTerminalで一般ユーザーとして実行する（MacBook Proの例。Airは `macbook-air-setup.sh`）。Macからのcloneには、Giteaのユーザー名とパスワードを聞かれる（NASからは匿名で取得できる）。目安20〜40分（未測定）で、完了表示までCtrl+Cを押さない。

```sh
d="$HOME/Workspace/issue-desk-intake"; if [ -d "$d/.git" ]; then git -C "$d" pull --ff-only; else git clone http://192.168.0.10:3000/mofua-suki/issue-desk-intake.git "$d"; fi && sh "$d/deploy/clients/macbook-pro-setup.sh"
```

Xcode本体の選択と初回設定を先に確かめ、足りなければ何も入れずに止まる。そのあとGo 1.27.0（`~/.issue-desk/tools/go`、チェックサムを照合）・Flutter 3.47.9（`~/development/flutter`、既にある別の版は上書きしない）・`~/.zprofile` のPATH・状態フォルダー（`~/.issue-desk/apple`）・`~/Workspace/sample-expense-client` のcloneを用意する。精算Clientのテストは流さない（下のNASからの操作で流す）。何度実行してもよい。ホームが登録と違うときは注意を表示するので、稼働させる前に `fleet.json` の値を合わせる。

### MacBook ProのテストをNASからSSHで流す

MacBook Proはホスト作業用SSH（`host-ssh.json` のPC名 `macbook-pro`、接続名 `user-macbook-pro.example.ts.net`、利用者 `user`）に登録してある。操作は `status`（読み取り専用で自動承認）と `verify-sample-expense-client-ios`。後者は、NASが標準入力で送る精算Clientの作業ツリーのtar.gzを一時フォルダーへ展開し、iOSの実行環境が無ければ `xcodebuild -downloadPlatform iOS` で入れ、起動済みのシミュレーターが無ければ使えるiPhoneのシミュレーターを1台起動して、Macに入れたFlutterで `sh scripts/verify-apple.sh ios`（Xcodeでのシミュレーター向けビルドと、シミュレーター上のログイン画面のテスト）を流し、最後に `verify=<終了コード>` を出す。自分で起動したシミュレーターは終わったら止める。独自lint・Masonの検証と、Androidを含むそのほかのテストはLinuxの担当PCで流す。送ったコードを実行するので自動承認せず、利用者の発言を引用した `host-ssh` の許可付与 `macbook-pro:verify-sample-expense-client-ios` が要る。

`probe-worker-apple-ios` は、標準入力の tar.gz（`worker/` にWorkerのソース、`client/` に精算Clientの作業ツリー）を一時フォルダーへ展開し、MacのGoでWorkerをDarwin向けにビルドして `worker apple probe --config ~/.issue-desk/apple/apple-k2.json --repo mofua-suki/sample-expense-client --tree <client>` を流す。runner本体と同じ隔離の規則（Workerのコード）で、隔離の検査の結果を出し、受付が配った固定のtestCommand（`sh scripts/verify-apple.sh ios`）を1回流して、拒否された操作をプロセス名つきで出す（`log show` の `deny(`）。Workerの隔離の規則を直すときに、直した版のソースを送ってMacに入れ直さずに確かめるのに使う。検証要求の引き取りと受付への送信はしない。許可付与の対象は `macbook-pro:probe-worker-apple-ios`。NASからの待ち時間は3500秒。MacはGiteaから取得しないので、SSHのセッションでGiteaのログインが要らない。送るtar.gzの作り方は [chat-operations.md](chat-operations.md) の「SSHで精算アプリの固定テストを別PCで流す」と同じ（`--host macbook-pro --operation verify-sample-expense-client-ios`）。

`deploy-worker-apple-runner` は、MacBook Proに常駐するrunner（`worker apple run`）を、NASが送ったWorkerのソースで入れ替える。標準入力の tar.gz（`worker/` にWorkerのソース、`runner-setup.sh` に `deploy/clients/macbook-pro-runner-setup.sh`）を一時フォルダーへ展開し、`sh runner-setup.sh --source <worker>` を流して最後に `deploy=<終了コード>` を出す。セットアップと同じく、接続設定（K2）を書き、Workerをビルドして `~/.issue-desk/apple/bin/worker` に置き、`worker apple check` が通ったときだけ常駐させる（常駐中なら再起動、未登録なら `prepare.sh --start`）。SSHのセッションではGiteaのログインができないので、Mac上の `git pull` を使わない。WorkerのMac向けの変更をマージした後、送るソースはWorkerの最新のtrunk（`git archive origin/trunk`）にする。送ったコードを常駐させるので自動承認せず、許可付与の対象は `macbook-pro:deploy-worker-apple-runner`。NASからの待ち時間は1800秒。受付のPC別トークンとGiteaの資格情報は、利用者が置いたものをそのまま使う（この操作は読まず、置き換えもしない）。

受け入れの準備は、MacのTerminalでふだん使う利用者から1回だけ実行する（管理者のパスワードを聞かれる。目安1〜2分、完了表示までCtrl+Cを押さない）。Giteaのファイルを直接取る `raw` のURLはMacからは404になったので、Gitで取得する（Giteaのユーザー名とパスワードを聞かれる）。コマンドラインツールの導入を求められたら入れてから実行し直す。

```sh
d=$(mktemp -d) && git clone -q --depth 1 http://192.168.0.10:3000/mofua-suki/issue-desk-intake.git "$d/i" && sudo sh "$d/i/deploy/clients/mac-ssh-accept.sh"; rm -rf "$d"
```

`deploy/clients/mac-ssh-accept.sh` は、NASの署名役のCA公開鍵（`/etc/issue-desk/ssh-repair-ca.pub` と同じ値）・principal `issue-desk-repair-macbook-pro`・`/etc/ssh/sshd_config.d/050-issue-desk.conf` を置き、リモートログインを有効にする。パスワードや個人の鍵の設定は変えない。既存の設定に同じ指示があれば上書きせずに止まる。実行した利用者が登録と違うときは、その名前を表示する。その場合は `host-ssh.json` の `user` を直すまで接続できない。Xcodeのライセンス同意と初回設定（`sudo` が要る）はSSHから行わないので、セットアップがそこで止まったらMac上で済ませる。シミュレーターはMacに画面でログインしていないと起動できないことがある（推定）。

### 停止中のMacを稼働させる

1. Mac上でセットアップが「完了」まで成功していることを確認する。
2. 検証させるMacを1台選び、`mofua-suki/sample-expense-client` の全実装担当（`fleet.json` の各ホスト）の同リポジトリ設定に `"nativeTestHost": "<そのID>"` を付け、Gitea上のテスト専用アカウントを `assignments.workerAccounts` に足す。`nativeTestHost` は1リポジトリに1台だけ。`test/mac-registration.test.ts` の停止中の確認もあわせて直す。
3. 下の「トークンと起動」に従ってrunnerを起動してからPRをマージする。

## トークンと起動

MacBook Proでは、トークンを置いた後に `sh ~/Workspace/issue-desk-intake/deploy/clients/macbook-pro-runner-setup.sh` を実行する。接続設定（`~/.issue-desk/apple/apple-k2.json`）を書き、Workerを `~/Workspace/issue-desk-worker` からDarwin向けにビルドして `~/.issue-desk/apple/bin/worker` に置き、`worker apple check` が通ったときだけ `prepare.sh --start --role test-only` でLaunchAgentに常駐させ、同じ定義を `~/Library/LaunchAgents` にも置いてログイン時に起動させる。2回目からは、Workerをビルドし直して常駐中のrunnerを再起動する（再起動などで登録だけが消えていれば、準備済みの定義を読み込み直す）。このスクリプトはトークンを読まない。Giteaの資格情報は `fleet.json` の `workerTokenFile`（`read:user`・`read:repository`）に置く。

PC別トークンの発行は受付の画面ではできず、NASの管理コマンド（`sh ~/Workspace/issue-desk-intake/deploy/nas/intake.sh exec -T /usr/local/bin/intake admin --config /state/config.local.json --application-dir /app pc-token issue <ID>`）で行う。値は1回だけ標準出力に出るので、MacからNASへSSHして実行し、その出力をMacの `~/.issue-desk/apple/private/pc-token`（所有者だけが読める）へ直接保存する。ネイティブテストIDと実装用IDに別々のものを使う。受付設定は `serverUrl`・`serverTokenFile`・`workerId` の3項目。秘密は引数やGitへ入れず、所有者だけが読めるファイルに置く。

ネイティブrunnerには `assignments.workerAccounts` に生成したテスト専用アカウントと、そのアカウントの対象リポジトリを読み取れるGitea資格情報も必要。実装用には別のアカウントを使う。配置先は生成したfleet設定の `workerTokenFile`。テストする作業ツリーとは別に置く。実装用IDには通常Workerと同じGit・AI認証・状態volumeが必要。WorkerリポジトリのMac導入手順を使い、ネイティブrunnerと実装用コンテナを別々に起動する。

## 検証要求と結果

実装Workerは、公開済みの変更 `head`・取り込み先 `base`・マージ予定の内容 `tree` を固定して `native_test` を受付へ送る。Macは自分宛てで `testRepositories` に登録された要求だけを引き取る。コマンド・リポジトリURLは送信者の要求から受け取らず、受付が配った自分の設定を使う。

`passed`・`failed`・`unrunnable`・`interrupted` の結果には3つのSHAと3000文字以内の `message` が必要。Macテスト設定のダイジェストも実装担当へ配布し、SDK・コマンド・ネットワーク設定の変更後は以前の結果を再利用しない。異なるSHA、別PCからの結果、担当変更後の結果、確定済み結果の書き換えを拒否する。Macの未起動・失敗・未実行・時間切れを成功にせず、実装担当はマージを進めない。

PC一覧には「テスト専用」「実装担当」と担当リポジトリを表示する。要求の状態は既存の `/api/v1/requests/<ID>` で送り手または対象Macが確認する。

## 登録解除・復旧

Macを止める前に、そのMac宛ての検証要求と依存するマージを完了させる。検証が必須のままrunnerを止めるとマージ待ちが残る。検証先を変更するときは同じリポジトリの全実装担当を揃え、進行中要求を現在の設定で出し直す。受付が変更後に旧担当の結果を受理することはない。旧担当の要求は次の引取対象から外す。引取済み要求のreleaseと未引取要求のcancelは担当変更後も可能。古い設定の読取結果で有効な新要求を終了させないため、一括の自動取消は行わない。

DBの追加は `native_test_requests` 表だけで、既存要求を変更しない。更新失敗時は通常のDBバックアップを使って復旧する。旧版へ戻す場合もこの表を残したままにし、新しい検証要求を旧版Workerへ渡さない。表の削除は復旧に必要ない。
