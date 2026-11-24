---
name: merge-manual-pr
description: issue-deskの監視が担当するリポジトリで、利用者が明示した直接PR・AIセッションのPRを作成・マージするときに使う。push不可時のパッチ、状態照会・取消、直接PRで実装したIssueのクローズも扱う。
---

<!-- skill-standard: 1 -->

# 直接PRの作成・マージ

同じ対象の直接PRの明示は、PR作成・push・検証・通常マージ・実装完了した該当Issueのクローズを含む。検証成功・依頼範囲への一致を確かめ、既存の了承を聞き返さず最後まで進める。検証失敗・対象や範囲の変更は条件の変化。削除・権限保護・SSH・対象SHAなどの個別承認は代替しない。可逆性だけを未許可操作の許可にしない。

「任せる」「直して」だけでは直接PRの許可にしない。通常の依頼は [issue-desk-operations](../issue-desk-operations/SKILL.md) から起票する。固定SSH登録の追加には [host-ssh-permissions](../host-ssh-permissions/SKILL.md) の直接PR方針を適用する。受付停止時の例外は `chat-instructions.md` の直接PRの項に従う。Issueの読取・終了条件は [issue-desk-issue-control](../issue-desk-issue-control/SKILL.md) に従う。

## 公開の順序と検証

- 専用ブランチを作り、既定ブランチへ直接pushしない。公開前にfetchし、基点・入るコミット・差分が今回の対象だけであることを確認する。
- push前に、そのリポジトリのAGENTS・READMEが定める静的解析・整形・テストを全部通す。変更箇所だけの検証や、マージ前の固定テストで代えない。issue-desk-intakeは `sh scripts/verify.sh`。NASでこのリポジトリを検証するときは、担当PCへ送る検証に `--direct-pr <対象コミットSHA>` を付ける。道具が見つからなければ [nas-verify](../nas-verify/SKILL.md) を読む。未検証・不合格のままPRやマージ依頼を送らない。
- PR本文は、問題・最終的な変更・実行した検証と結果を書く。実行していない検証を成功と書かない。
- マージ前に、差分だけを渡した別のサブエージェントで独立レビューする。実装側の結論を渡さない。指摘を直したら検証と変更後の再レビューを行う。対象コミット・結果・対応を会話へ残し、PR作成結果の実headと一致を確認して依頼する。

## 既存PCトークンの窓口を使う

NASは `sh ~/.issue-desk/desk-pc get|operation`、Ubuntuは `~/.issue-desk/desk get|operation`。同じPCの稼働WorkerのK2を窓口が読む。トークンの値を表示・複製せず、既存の `doctor` で接続を確認する。窓口には `--config` を足さない。[共通API手順](../issue-desk-api/SKILL.md) の応答 `status/body/bodyType/redacted` と失敗の扱いに従う。

PR依頼の正本は `api/openapi/41-merge-turns-requests.yaml` の `/api/v1/requests`。認証はPCトークンだけで、依頼そのものには運用代行の追加許可は不要。保護対象を承認する場合の運用代行など、対象操作の条件は別に検査する。NASの一時キー・画面セッション・別アカウントへ切り替えて403を回避しない。

具体的なJSON、パッチ生成、ID保存、状態照会・取消は [PR依頼のAPI手順](refs/request-api.md) を読む。作成・マージはどちらも同じAPIへの非同期依頼で、POST成功をPR作成・マージ完了としない。

相談開始・回答・確定・実装AIへの返信も、[issue-desk-operationsの非同期手順](../issue-desk-operations/SKILL.md#受理と完了を分け同じ識別子で待つ)で受理と完了を分ける。PR作成・マージはこのSkillのrequest.idで待機し、相談の下書きID・返信の送信ID・共通番号と混同しない。

## 道具で送って結果まで待つ

使い捨てのスクリプトを書かず、`scripts/direct-pr.ts` を使う。refs の手順（IDを作る→依頼JSONを保存→POST→応答の照合→同IDのGETで `done` まで待つ→結果の照合）をそのまま行う。NASは `sh ~/.issue-desk/desk-pc direct-pr.ts`、Ubuntuは `~/.issue-desk/desk direct-pr.ts` で呼ぶ。`--save` は、まだ無いファイルの絶対パスで、作業ツリーの `.tmp/` に置く。ホームの下に無いパスは断る（実パスで判定。窓口のコンテナの中に保存されて消えるため）。

```sh
sh ~/.issue-desk/desk-pc direct-pr.ts create --repo <owner/repo> --save <作業ツリー>/.tmp/pr-create.json --title '<題>' --body-file <作業ツリー>/.tmp/pr-body.md --branch <pushしたブランチ> --expect-head <pushした40桁>
sh ~/.issue-desk/desk-pc direct-pr.ts merge --repo <owner/repo> --save <作業ツリー>/.tmp/pr-merge.json --number <作成結果の番号> --head <作成結果のhead>
sh ~/.issue-desk/desk-pc direct-pr.ts resume --save <作業ツリー>/.tmp/pr-create.json --expect-head <pushした40桁>
sh ~/.issue-desk/desk-pc direct-pr.ts resume --save <作業ツリー>/.tmp/pr-merge.json
```

- pushできないときは `--branch` の代わりに `--patch-file <パッチ> --base <40桁の基点>`。パッチの作り方は refs のとおり。SHA256は道具が計算し、結果の `patchSha256` と照合する。
- 個別承認が要るマージだけ、`--approval '<利用者の発言の原文>'`（記号の回答なら `--approval-question '<見せた質問文>' --approval-answer <記号>`）と `--instruction-at <その発言の日時>` を付ける。片方だけ・定型語だけ・未来や7日より前の日時は、接続する前に断る。
- PR番号が決まる前に利用者が承認したとき（事前承認）は、同じ `--approval`（または `--approval-question`・`--approval-answer`）と `--instruction-at` を `create` に付け、マージでは承認の代わりに `--approval-request <その作成依頼のID>`（保存した作成依頼JSONの `id`）を付ける。受付が作成依頼の結果（同じrepo・送り手・PR番号・head）と運用代行トークンの承認を照合して、承認をマージ依頼へ写す。合わなければ400で断られる。承認と `--approval-request` は同時に付けない。
- ブランチ方式の作成は、`create` でも `resume` でも `--expect-head` が必須。作られたPRのheadと照合する。
- 終了コード0だけが作成・マージの完了。1は拒否・失敗・取消・照合の不一致で、標準エラーの理由を読む。2は引数の誤りで、何も送っていない。75は待ち切れなかった・受理不明で、新しい依頼を作らず同じ `--save` で `resume` する。待ちは既定60分（`--wait-minutes` で最大720分）。
- 取消は道具に無い。refs の手順で同じIDへ送る。依頼JSONは用が済んだら消す。

## PR作成からマージ完了まで

1. ブランチ方式では同じリポジトリへpushしてから `kind: "pr_create"` を送る。pushが断られた場合だけパッチ方式へ進み、新しい認証や権限を足さない。Issueの `codex/issue-*` と別リポジトリのブランチは対象外。
2. 依頼IDと本文を送信前に保存し、同IDで結果を確認する。作成済みは `request.status: "done"` かつ `result.status: "created"`。実番号・URL・40桁head、パッチ方式ではSHA256も照合する。`permissionHold` があれば保護対象を確認する。
3. 同じ対象の通常マージが許可済みで、検証・独立レビューと範囲が一致し、個別承認の条件も満たすときは、返却された実番号とheadで `kind: "merge_manual"` を送る。番号やheadを推測しない。
4. 同IDで `done` と `result.status: "merged"` を確認するまで続ける。queued/claimedは未完了。読取失敗・不正応答を成功や未処理と扱わず、新しいIDで重ねない。

ワーカーは、そのリポジトリの実装・レビュー・配置とマージの番を調整し、既存処理の合間にマージする。依頼は受付の共有DBに置かれ、implementersにいるPCが引き取る。各PCのruntimeへファイルを置いたり、GiteaのUI/APIで直接マージしたりしない。例外は、ワーカー停止を確かめ、利用者が今すぐのマージを明示したときだけ。

## 個別承認と失敗

- 権限ファイル・データ削除SQLの変更は、利用者が対象の保護対象操作とhead SHAを特定して承認した発言を `payload.approval`、会話にあるその発言の日時を `payload.instructionAt` に入れる。PR作成前の承認も対象を特定できれば、作成依頼の `payload.approval`・`payload.instructionAt` に入れ、マージ依頼の `payload.approvalRequest` で指して使う（事前承認）。AIの要約や判断で代えない。通常のIssue向け権限承認APIには送らない。記号回答は見せた質問文と回答を組にして原文のまま記録する。
- 承認発言は意味のある8文字以上・500 Unicodeコードポイント以内。指示日時は実在する秒付きISO 8601とUTCオフセットで、未来や7日より前を使わない。事前承認を使わずPR番号がない発言を送ってワーカーに断られた場合だけ、実番号とhead・対象操作を含む質問を1回する。番号を言い直してもらうためだけに既存の承認を捨てない。
- `refused` は拒否理由、`failed` は失敗原因を読む。条件を直した場合も、変更後の検証・独立レビューを通して、新しいheadで依頼する。固定テスト不合格なら出た失敗を手元で直し、既定ブランチを取り込んで全部の検証を通す。未整形なら定められたformatterを使う。pushできなければ整形済みパッチを別PRとして渡し、旧PRとの置換を記録する。
- 拒否が続く場合は別の経路に回り込まず、理由と、必要な個別承認条件を保った具体的な同API要求を示す。implementersが無ければ担当不在を報告し、Gitea画面のマージを代わりに頼まない。
- 期限経過は直接PR・マージ・個別承認の代わりにならない。

## 起票済みの依頼から切り替えたとき

利用者が、起票済みのIssueや進行中の相談と同じ対象を直接PRで実装すると明示したら、PRに着手する前に、そのIssueを停止し、相談を終える。止める指示を別に待たない。実装AIが同じ対象を並行して実装し、利用枠を無駄にするのを防ぐためである。

- Issueの停止: [停止・再開](../issue-desk-issue-control/refs/api-operations.md) の手順で、POST `/api/comment` に `action: "stop"` を送る。
- 相談の終了: POST `/api/drafts/<下書きID>/finish` で、起票せずに終える。PC別トークンでは、利用者の指示の日時を `instructionAt` に入れる。
- 停止・終了が断られたら理由を読む。実装中なら止まったことを確かめてから着手し、PR作成済みや終了済みなら、そのPR・結果と重ならないかを確かめる。
- 対象の一部だけを直接PRにするときは、止める前に、残りを別の依頼として残す（[issue-desk-operations](../issue-desk-operations/SKILL.md) の起票）。
- 全て実装してマージしたら、下の「マージ後」どおり `other_route` で閉じる。PRを取りやめた場合は、同じ手順の `resume` で再開し、終えた相談は出し直す。

## マージ後

受付のIssueをこの直接PRで全て実装した場合は、マージ成功後に [Issue終了の手順](../issue-desk-issue-control/refs/close-issue.md) で `reason: "other_route"` と根拠の `owner/repo#PR番号` を添えて閉じる。一部だけなら閉じず残件を保つ。マージ前には閉じない。

反映はNASのsystemdタイマーと各PCのhostd・rc-updateなど既存のホスト更新で確認する。対象SHA・稼働版・SkillやCLIの配布版・更新結果を照合し、マージと反映を区別する。すぐ反映されなくても手でpullや再起動をしない。条件と版の詳細は [チャット配布](../../docs/chat-operations.md) と [change-ordering](../change-ordering/SKILL.md) を参照する。
