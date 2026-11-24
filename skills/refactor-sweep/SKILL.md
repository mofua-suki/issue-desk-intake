---
name: refactor-sweep
description: リファクタリング・コードの点検（使われないコード・引数、責務の混在、重複、手書きのJSON読み、JavaScriptの残り）を頼まれたときに使う。道具の固定版、観点の選び方、承認が要る変更、入口の改名の段取り。
---

<!-- skill-standard: 1 -->

# リファクタリングの点検

完了の基準が無い依頼なので、[task-completion](../task-completion/SKILL.md) の `kind: "open-ended"` で台帳を持ち、差分を知らない見直しを指摘が無くなるまで繰り返す。不要になったファイル・テストは [obsolete-cleanup](../obsolete-cleanup/SKILL.md) に従って同じ変更で消す。

## 観点

- 使われないコード: 道具の検出を起点に、呼び出し元を `git grep` で確かめてから消す。文字列で組み立てる呼び出しや配布・資料からの参照も見る。
- 使われない引数: 引数だけでなく、渡している呼び出し側ごと消す。ただし、OpenAPIのパラメータは残す。
- 責務の混在: 行数ではなく責務の境界で分ける。長いだけのファイルは分けない。1つのファイル・関数が別々の理由で変わるなら分ける。
- JSONの列を手で読む: 同じ形の取り出し・型の確かめを何か所も書いていたら、既存の共通部品へ寄せる。無ければ1つ作って全部を寄せる。
- 重複: 同じ処理の写しは1つにする。テストの重複は obsolete-cleanup の条件で消す。
- JavaScriptを残さない: 手書きの `.js`・`.mjs` を見つけたら TypeScript へ移す。

前回の点検と同じ観点・同じ道具の結果に偏らない。過去の点検の作業ツリーや報告を手本にせず、今回は別の観点から始める。

## 道具（Go）

作業ツリーの `.tmp/gobin` へ固定版で入れる。`@latest` は使わない。Goの道具は `GODEBUG` が残っていると起動直後に止まるので、外して呼ぶ。

```sh
mkdir -p .tmp/gobin
ok=1
for tool in golang.org/x/tools/cmd/deadcode@v0.52.0 honnef.co/go/tools/cmd/staticcheck@v0.8.2 github.com/mibk/dupl@v1.1.0 github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0; do
  GOBIN=$PWD/.tmp/gobin ~/.issue-desk/go install "$tool" || { echo "入れられません: $tool" >&2; ok=0; break; }
done
[ "$ok" = 1 ] && cd server && {
  env -u GODEBUG PATH=$HOME/.local/bin:$PATH ../.tmp/gobin/deadcode -test ./...
  env -u GODEBUG PATH=$HOME/.local/bin:$PATH ../.tmp/gobin/staticcheck ./...
  env -u GODEBUG ../.tmp/gobin/dupl -t 100 .
  env -u GODEBUG ../.tmp/gobin/gocyclo -over 30 .
}
```

- 4つとも上の版で入れ、Go 1.27 の `server/` で動かして確かめた（deadcode・gocyclo は全体、staticcheck・dupl は一部のパッケージ。staticcheck の `-version` は `2026.2.2 (0.8.2)`）。入れられないときは版を替えず、止まった理由を報告する。
- 検出は候補にすぎない。消す・分ける前に参照と挙動を確かめ、テストで守る。
- 用が済んだら `.tmp/gobin` を消す。

## 承認が要る変更

権限保護のファイルとデータ削除のSQLの変更は個別承認が要る。点検の流れで混ぜず、別のPRに分けて [merge-manual-pr](../merge-manual-pr/SKILL.md)『個別承認と失敗』に従う。

## 入口の改名

配布されて別のPC・チャットから呼ばれる入口（スクリプト名・引数・APIの経路・Skill名）は、1回の変更で名前を替えない。次の順に別々のPRで進め、各段の配布を [change-ordering](../change-ordering/SKILL.md)『マージと反映の時刻を分ける』で確かめてから次へ進む。

1. 両方の名前を受け付ける。
2. NAS・各PCへの配布を確かめる。
3. 呼ぶ側・資料を新しい名前へ替える。
4. 古い名前の受け付け（移行措置）を外す。
