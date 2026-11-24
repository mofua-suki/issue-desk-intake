# issue-desk-intake

> **このリポジトリについて**
> 個人で設計・運用している「AIに開発を任せる仕組み」issue-desk の受付側です。利用者がチャットやブラウザから相談すると、受付のAIが依頼を分割・整理してIssueにし、自宅の複数のPCで動く [issue-desk-worker](https://github.com/mofua-suki/issue-desk-worker) へ仕事を配ります。実装・テスト・独立レビュー・マージまでをAI（Codex / Claude）が行い、受付は進行・使用量・承認・要対応を管理します。
>
> - 構成: Go（業務API・永続化・AI相談）、TypeScript（ブラウザ画面）、OpenAPI（API仕様）、PostgreSQL、Docker
> - 実際にNAS上で常時稼働しており、テストは実PostgreSQLを使う結合テストまで含みます。
> - `skills/`・`chat-instructions.md` などは、AIに渡す運用規則そのものです。
> - 公開用に整理した版です。開発履歴は1コミットにまとめ、自宅の接続先・アカウント名などは例示値（`192.168.0.10`、`example.ts.net` など）に置き換えています。そのままでは手元の環境では動きません。


NASで動くServerとブラウザのClient。相談、仕事の割り当て、状態管理、認証、APIを担当する。各PCの実行処理とhostdは`issue-desk-worker`で管理する。

| 場所 | 内容 |
| --- | --- |
| `server/` | Goの業務API、永続化、AI相談、仕事の制御 |
| `client/` | TypeScriptのブラウザ画面とAPI通信 |
| `api/` | OpenAPI、通信型、契約の検証データ |
| `deploy/nas/` | NASの配置・更新・バックアップ |
| `deploy/clients/` | Linux実行PCの導入・チャット補助の配布 |
| `scripts/` | API経由の管理操作と検証 |
| `skills/` | チャット用の操作手順と受付AIの相談手順 |

## 開発と検証

Go 1.27、Node.js 24、pnpm 11.26.0を使う。次のコマンドは検証担当PCの作業ツリーで実行する。NASのチャットから全体検証を依頼するときは、[nas-verify](skills/nas-verify/SKILL.md) に従って担当PCへ送る。

```sh
sh scripts/verify.sh
```

固定検証はServerの整形・テスト・静的検査とrace検出、Clientの型検査・テスト・ビルド、管理スクリプトの検査を実行する。race検出にはCGOとCコンパイラが必要で、対応するLinux環境ではコンパイラを自動準備できる。単独のテストは `sh scripts/with-run-tmpdir.sh <コマンド>` で隔離した一時領域を使える。

Goの未使用コードと静的検査にはdeadcodeとstaticcheckを使う。TypeScript/JavaScriptの整形を個別に確認するには `node scripts/check-ts-quality.ts format` を実行する。lintは最後の引数を `lint`、未使用exportの検査は `exports` に替える。生成物は固定検証で再生成結果との一致も確認する。

契約の正本は `shared/src/desk-contract.ts`。`node scripts/generate-desk-contract.ts --write` で配布用moduleと `chat-instructions.md` 内の契約生成範囲を同期する。固定検証は書き込まず再現一致を確認する。

`ISSUE_DESK_TEST_POSTGRES_DSN` を省略すると、固定検証は隔離されたPostgreSQL 17.11と専用DBを自動準備する。Linux x64/arm64の一般ユーザー、npmレジストリへの接続、tarが必要。バイナリは固定SHA-512で照合するが、配布元の信頼性は別途確認が必要。取得済みの同版の実行ファイルを使う場合は、postgres・initdb・pg_ctlのあるディレクトリを `ISSUE_DESK_TEST_POSTGRES_BIN` に指定できる。第三者のnpm配布物を使えない場合は専用検証DBとクライアントを別途用意する。

一時DBはループバックの空きポートだけで待ち受け、成功・失敗・SIGINT/SIGTERM時に停止して一時領域を削除する。既存の検証DBを使う場合は、DB名が`issue_desk_test`で始まる破棄可能な専用DBのPostgreSQL URLを`ISSUE_DESK_TEST_POSTGRES_DSN`に指定する。不正な指定は自動DBへ切り替えず失敗する。本番の接続先・資格情報・状態ボリュームを渡さない。

注意点分類と起動・停止・AI枠の固定テストは実PostgreSQLが必須で、準備失敗・SKIP・テストの欠落は検証失敗とする。`scripts/verify-review-notes.ts`がキャッシュなしで実行し、対象テストごとのPASSを出力する。通常の`go test ./...`だけではDB未指定時に結合テストがSKIPになるため、固定検証の代わりにはならない。また固定検証はNAS健康照合のNode経路をキャッシュなしで実行し、対象サブテストのPASSがない場合は失敗する。

受付の設計は [API契約](docs/api-compatibility.md)、[相談の範囲](docs/consultation-spec.md)、[受入条件](docs/consultation-acceptance.md)、[受入条件の実行計画](docs/execution-plan.md) を参照する。

個別の計測手順は [利用量の通知](docs/usage-review-alert-measurement.md)、[相談からマージまで](docs/consultation-throughput-measurement.md)、[SSH申請の期限切れ](docs/ssh-expiry-measurements.md)、[起票比較資料の祖先読取](docs/filing-material-measurements.md) にある。祖先読取の大規模計測は明示指定で実行する。

Serverを手元で起動する場合は専用の設定とDBを用意する。

```sh
cd server
go build -o bin/intake ./cmd/intake
./bin/intake --config /absolute/path/config.local.json --application-dir .. --assets ../client/dist
```

待受アドレスは設定ファイルの`intake.bind`と`intake.port`で指定する。ClientはServerと同じOriginで配信する。静的ファイルとキャッシュの仕様は[client/README.md](client/README.md)を参照。

ホスト作業用SSHの `host-ssh.json` には、任意の `operationHosts` を操作名からPC名の配列への対応として指定できる。指定した操作は登録済みの列挙されたPCに限り、省略した操作は従来どおり全登録PCを対象とする。指定する対応と各配列は空にできず、未登録の操作・PCやPCの重複は設定エラーとなる。許可の登録・反映、申請・個別承認・発券時に現在の方針を確認する。

### 実装難易度の自動判定

受付は起票時に次の順で決める（`server/internal/filing/tier.go`）。

1. 依頼者が明示した難易度
2. 止まりの修正 → standard
3. 複数プロジェクトの案、または分けられない理由（heavyReason）がある → heavy
4. 有効な変更範囲の宣言（1〜40パス）と1件以上の受入条件がある → light
5. それ以外（変更範囲の宣言なし・宣言が不明・受入条件なし）→ standard

変更範囲の宣言が不明なだけでは heavy にしない（小さな依頼が混ざるため）。PRの軽微な指摘への対応（後続案）も同じ規則で決め、案の変更範囲と指摘ごとのテスト方法を宣言と受入条件として扱う（[後続案](docs/followup-proposals.md)）。受入条件の具体的な確認手段と期待結果は受付時に検査する。light の件数や見込み規模に上限は設けない。

`assignments.json` の共通設定 `models.implementation.disableLightTier` を `true` にすると、受付が自動で light と判定する依頼を standard で起票する。省略または `false` では light の判定をそのまま使う。依頼者が明示した難易度、止まりの修正、heavy の優先順位は変わらない。共通 `models` がある場合、このキーの省略はローカル設定を消して既定値に戻す。

[issue-desk-stats Skill](skills/issue-desk-stats/SKILL.md)で設定変更前後の手戻りやレビューを比較するときは、期間・件数・欠測・設定と配置版を記録する。standard の母集団は変更され、明示指定の light や難易度不明も残るため、前後差だけで設定の因果効果とは判断しない。

## 保存と認証

仕事・lease・配布状態・相談・認証・設定はPostgreSQLに保存する。共有DBとアプリ用ストアは接続経路が分かれ、アプリ用ストアは `issue_desk_app_migration` スキーマを使う。接続先は `ISSUE_DESK_DATABASE_URL` または `PGHOST`・`PGUSER`・`PGDATABASE` で指定し、接続先がなければ起動しない。PostgreSQLに `app_store_cutover` の記録がない場合は起動を止める。DBへ接続するのはServerだけで、WorkerとClientはAPIを使う。適用済みスキーマは保持し、追加のマイグレーションで更新する。

`runtime/state` の番号台帳（`intake-desk-numbers.jsonl`）は今も使うので消さない。切替の記録が無いときの復旧は `docs/pg-cutover.md`。

PC別トークン、hostd用トークン、画面セッション、一時キーの権限を区別する。認証失敗時に別の認証方式へ戻らず、画面の変更操作はOriginとCSRFを検査する。ローカル管理CLIとチャット補助の配置は[docs/chat-operations.md](docs/chat-operations.md)を参照。

別プロセス（`intake admin` の一時キー発行など）も同じPostgreSQLを使う。書き手の排他は共有DBのリース（`server/internal/leadership/leader.go`）だけが担う。

## NASへの配置

遅延有事の起票結果が不明な場合は[照合と引継ぎの手順](docs/slow-incident-recovery.md)を参照する。

`deploy/nas/.env.example`をもとに配置先専用の`.env`を用意する。既存のPostgreSQL・状態・認証ボリュームとComposeプロジェクト名を保持する。CLIツールのベースはWorkerリポジトリの`Dockerfile.tools`からビルドした固定版を`ISSUE_DESK_TOOLS_IMAGE`に指定する。Codexは `deploy/nas/Dockerfile` の `CODEX_TOOLS_IMAGE` で固定したWorkerのツールイメージから導入し、受付イメージの構築時に `deploy/versions.json` の版と照合する。Codexを更新するときは、その版を含むWorkerのツールイメージを発行してからこの2つの指定をそろえる。受付の実行時のNodeはツールイメージから写さず、`deploy/nas/Dockerfile` の最後の段の基本イメージ（`deploy/versions.json` の `node` と同じ版）を使う。NAS本体の `~/.local/bin/node`（npm・npx・corepack・pnpm・pnpxを含む）も、`deploy/nas/update.sh` が `deploy/nas/update-host-node.sh` で同じ版にそろえる（nodejs.orgの配布物をSHASUMS256で照合して `~/.local/lib/nodejs/node-v<版>-linux-<arch>` に展開し、`current` を切り替える。前の版は消さない）。OS付属の `/usr/bin/node` は対象外。

```sh
sh deploy/nas/update.sh --dry-run
sh deploy/nas/update.sh --intake-now
```

更新はホストから実行し、待機色の確認と処理完了後に書き手を交代する。入口の確認に失敗したら旧色へ戻す。未受理の印のない失敗は到達したか不明なため、送信IDで照合できる経路だけ再送する。交代中の要求と一時キーの扱いは[交代時の要求処理](docs/chat-operations.md#交代時の要求処理)を参照。`deploy/nas/intake.sh status` で稼働色とrevisionを確認できる。

共有DBの設定変更は両色の停止を伴う。[共有DBの設定保守](docs/db-settings-maintenance.md) に従い、通常のServer更新と別に実施する。

交代失敗時は書き手の多重化を避け、[復旧手順](docs/chat-operations.md#交代時の要求処理)に従う。交代後は要対応（`node scripts/desk-alerts.ts`）も確認する。

## 実装担当と配布

リポジトリごとの実装担当と順位は [assignments.json](assignments.json) の `hosts.<PC>.repositories`、PC全体の同時実装上限は `maxImplementations` で確認する。NASは受付の稼働先。一般アプリの自動配置と、受付・Worker自身の更新にはそれぞれの更新経路を使う。

受付は起票前に `fleet.json` の `hosts.<PC>.environment` を、対象リポジトリの `assignments.json` 実装担当と照合する。fleetのリポジトリ設定だけでは実装担当にならない。例えば `{"platform":"linux","gcc":true,"cgo":true,"race":true,"docker":null,"realDevice":null,"liveMeasurement":null,"network":"allowlist"}` と宣言する。`platform` は実行環境を示し、実装Workerは `linux`、MacネイティブテストIDは `darwin`、未確認は `null`。実装用のLinuxコンテナとMac本体のネイティブテストは別IDで登録する。Macをテスト専用・実装兼用に分ける登録方法は [Macの役割別追加](docs/mac-roles.md) を参照。`gcc`・`cgo`・`race`・`docker`・`realDevice`・`liveMeasurement` は真偽値または `null`、`network` は `allowlist`・`none`・`null`。欠落または `null` は未確認であり、ホスト名や実行ファイルから能力を推定しない。`realDevice` と `liveMeasurement` の真は対象を識別できないため、要求された実機や測定先の充足は未確認として扱う。偽は該当能力がないことを示す。一般的なPCや測定ツールの存在から能力を推定しない。networkは接続権限を付与せず、許可リスト外の要求は従来の検査で扱う。

必要な能力は各担当PCで個別に照合する。担当PCが空、能力の取得失敗、または結果を決められない未確認値がある場合、能力不足とは断定しない。全担当PCで満たせない条件や一部PCだけで満たす条件は、起票時にPCを固定できないため原則「マージ後の運用作業（自動化の対象外）」へ移し、担当・手順・確認・自動化できない理由を残す。依頼の価値に不可欠な条件は利用者に範囲や完成物の選択を確認する。修復後も問題が残れば未確定とする。

`assignments.json` の `monitorHost` は監視PC、`releaseCanaryHosts` はカナリア候補の優先順を指定する。未設定の場合はそれぞれ監視なし、カナリアの段階を踏まずに配布する。

自動再実装の使用量判定と影響の確認手順は[使用量判定の計測](docs/usage-review-alert-measurement.md#自動再実装の使用量判定)を参照。

### テスト変更の並列着手

変更範囲にはテストも宣言し、範囲の拡大・実変更の照合・監査記録に残す。配布時の実行中ジョブとの重なり、待機予約、作業中の範囲拡大では、テストのパスだけが重なる場合は待機しない。製品コードが重なる場合は従来どおり待機する。

テストはファイル数の目安と同じ分類を使う。`_test.go`、`.test.`・`.spec.`を含むファイル名、`test_*.py`・`*_test.py`、`test`・`tests`・`testdata`・`__tests__` 配下が対象。テスト専用ディレクトリの宣言も対象だが、`*` や製品コードを含みうるディレクトリの宣言は除外しない。共通fixtureや削除・移動を含め、競合が消える保証ではない。マージの番と、最新の既定ブランチへの統合・検証は引き続き必要になる。

待ち原因の推定は現在の重なり規則で過去分も再計算する。変更前後の効果を比較する際は、当時の `dispatch_skip` などの記録と着手までの実時間も照合し、再分類だけを待ち時間の短縮と扱わない。

PC負荷標本の保持と画面への表示範囲は[稼働計測](docs/go-runtime-measurements.md#pc負荷標本の保持)を参照。
