---
name: nas-verify
description: NASでissue-desk-intakeなどの固定テスト（scripts/verify.sh、go test、pnpm）を自分で流すとき、重い検証を別PCへ送るときに使う。「pnpmが無い」「Goが無い」「no space left on device」「資格情報は所有者だけが読める」で止まったときも読む。
---

<!-- skill-standard: 1 -->

# NASで固定テストを流す

NAS上のClaude remote control・Codexでテストを始める前に読む。実装がNASでも、重い検証は担当PCへ送る。道具の有無と実行場所の選択を分ける。

- 全体の固定検証（`sh scripts/verify.sh` など）は、下の『重い検証は別PCで流す』で担当PCへ送る。
- NASで流すのは軽い個別テスト・整形確認だけ。全体のビルド・race検出や、個別でも大量のメモリ・一時領域を使う検証は担当PCへ送る。
- 別PCの許可待ち・接続失敗・未稼働をNASでの全体検証へ切り替える理由にしない。担当表の次順位を確認し、そのPCへの許可があれば送る。送れなければ検証を保留し、独立して進められる作業を続ける。

## 流し方

作業ツリーの根で実行する。出力が多いのでログへ書く。NASで許される軽い個別テストは `sh scripts/with-run-tmpdir.sh` で包む（下の『/tmp の空き』）。次は個別テストの例。全体検証は下のSSH入口を使う。

```sh
sh scripts/with-run-tmpdir.sh ~/.local/bin/node --test test/ssh-host-select.test.ts
```

- 環境は変えずに流す。`scripts/verify.sh` が自分で `~/.local/bin` の node・pnpm・go を選び、選んだ node を PATH の先頭に置き（`pnpm` は PATH の `node` で動き、既定の Node 18 では `requires at least Node.js` で止まる）、`GODEBUG` を外し（残っていると `removed GODEBUG "tlskyber"` でGoが起動直後に止まる）、umask を配置と同じ `022` にし、`node_modules` の作り直しの確認を出さない。
- 環境を変えないと通らないなら、変えて通すのではなく `scripts/verify.sh` を直す。
- 成否は終了コードで見る。パイプの `tail` などを挟むと `$?` は `tail` の値になる。

## pnpm

NASの `~/.local/bin` には、ホスト更新でNodeとともに固定版Corepackと `deploy/versions.json` の版のpnpmが用意される。起動しない場合は既存の更新経路を再実行し、次を作業ツリーの根で確認する。

```sh
~/.local/bin/node --version
~/.local/bin/corepack --version
~/.local/bin/pnpm --version
```

## /tmp の空き

`/tmp` は16GBのtmpfsで、ほかの作業と共有している。`df -h /tmp` を先に見る。

- `scripts/verify.sh` は `scripts/with-run-tmpdir.sh` を通して、作業ツリーごとの `/tmp/issue-desk-run-<作業ツリー名>-<番号>.XXXXXX` を `TMPDIR` にして流し、終わったら（失敗・TERMでも）中身ごと消す。テストが消し忘れた一時ファイルもここで消える。同じ作業ツリーの前回の実行が強制終了で残した物は、次の実行の始めに消す。ほかの作業ツリーの物には触れない。
- 軽い個別テストを単独で流すときも `sh scripts/with-run-tmpdir.sh ~/.local/bin/node --test test/ssh-host-select.test.ts` のように包む。

- `no space left on device`・`database or disk is full`・`disk I/O error` は、まず空き不足を疑う。修正の不合格と扱わず、空きを確かめて流し直す。
- `GOCACHE` を `/tmp` に置かない（既定の `~/.cache/go-build` のままにする）。
- `GOTMPDIR` と `TMPDIR` はホーム側へ移さない。`/home` はACL付きで、テストの一時ファイルの権限が変わり、`Giteaの資格情報は所有者だけが読める通常ファイルにしてください`・`permissions` で落ちる。これは修正の不合格ではない。
- 空きが足りないときは `GOFLAGS=-p=2` で同時に作るテストを減らす。
- ほかの作業の一時ファイルは、使用中か確かめずに消さない。

## 重い検証は別PCで流す

`sh scripts/verify.sh` 全体などの重い検証はNASで流さず、担当PCへ送る。

1. 送り先を確かめる: `~/.local/bin/node ~/Workspace/issue-desk-intake/scripts/ssh-host-select.ts --repo <owner/repo>`。送り先は担当表（`assignments.json`）の順位で決まり、自分で選ばない（issue-desk-intake は今は `gt12-pro`、次が `nab-9s`）。
2. NASからこのリポジトリの直接PRを検証する場合は、専用の `chat/` ブランチに変更をコミットし、次のコマンドに `--direct-pr <対象コミットSHA>` を付ける。担当表・受付の登録に一致する場合は個別の許可付与は不要。そのほかで、この会話で対象PCの検証SSHを許可済みなら聞き直さない。利用者の許可の発言と日時を引用し、`host-ssh` の対象 `<PC名>:verify` で許可の付与を登録する（[許可の付与](../issue-desk-issue-control/refs/grants.md)）。
3. `~/.local/bin/node ~/Workspace/issue-desk-intake/scripts/ssh-verify.ts --host auto --repo <owner/repo> --cwd <作業ツリーの絶対パス>` で流す。優先PCが停止中・利用できない場合、または同時に複数の作業ツリーを流す場合は、次順位のPCを `--host <PC名>` と名指しし、理由を `--reason` に書く（そのPCの許可も要る）。issue-desk-intakeはGT12を優先し、必要時にNAB-9Sを使う。
4. 終わったら許可の付与を取り消す。

- 成否は最後の `verify=<終了コード>` の行と入口の終了コードで見る。
- 接続先で動く固定コマンドは受付が配布物の `host-ssh.json` から渡し、実行部（`scripts/ssh-verify-host.sh`・`scripts/ssh-verify-runner.ts`）のSHA-256を埋め込んでいる。送る実行部は呼んだ配置先の `scripts/` のもの。実行部を変えたコミットが配置先に入り、受付のイメージがまだ古い間は、ハッシュの不一致で止まる。修正の不合格と扱わず、受付の更新を待つ。
- 接続先の動きとWorkerの固定テストとの差は [SSHでリポジトリの固定テストを担当PCで流す](../../docs/chat-operations.md#sshでリポジトリの固定テストを担当pcで流す)、許可を聞く形は [user-interaction](../user-interaction/SKILL.md)『別PCで固定テストだけを流す』。

## 報告

- 流した範囲を書く。段を分けた・一部を省いたなら、そう書く。
- 空き不足や置き場所で落ちた結果は不合格に数えず、流し直した結果を書く。

## 手元の罠

- シェルの `cp` は `cp -i` の別名で、上書きの確認で止まる。スクリプトでは `command cp -f` を使う。
