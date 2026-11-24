import { waitReplyRequest, type ReplyReceipt } from './reply-request.js';
import { executionProgressView } from './execution-progress.js';
import { finalApprovalView } from './final-approval-view.js';
import type {
  ProposalDismissalCandidate,
  BudgetOperation,
} from './attention-types.js';
import type {
  DetailCoverage,
  UsageFile,
  PromptPart,
  PromptParts,
  LimitReset,
  RateLimits,
  UsageArrival,
  UsageLimit,
  UsageLimits,
  FixedTestReport,
  FixedTestPeriod,
} from './usage-detail-types.js';
import type {
  ReworkTotal,
  ReworkPeriod,
  AverageMetric,
  ReviewNotesUsage,
  ReviewNotesPeriod,
  ReviewNotesReport,
  SelectionDuration,
  SelectionFixed,
  SelectionLeaks,
  SelectionLeakItem,
  SelectionPeriod,
  StallPeriod,
  StallDay,
} from './execution-stats-types.js';
import type {
  PerIssueMetric,
  ReturnRate,
  AntigravityGroup,
  AntigravityReport,
  IssueBodyGroup,
  IssueBodyReport,
  ScopePeriod,
  ScopeGroup,
  ScopeCheck,
  WorkTypeRate,
  WorkTypeGroup,
  WorkTypePeriod,
  InventoryRound,
  InventoryProject,
} from './comparison-stats-types.js';
import type {
  PriorityCondition,
  PriorityPopulation,
  PriorityChanges,
  PriorityDaily,
  PriorityReviewRound,
} from './priority-stats-types.js';
import type { OpsReport } from './ops-types.js';
import type {
  StatsReport,
  FollowupPeriod,
  UsageReviewRound,
  ReviewMeasure,
  ReviewExtra,
  TierOutcomeRow,
  TierReport,
  TierRateGroup,
  TierRatePeriod,
} from './stats-report-types.js';
import type {
  UsageAxis,
  MetricKey,
  UsageTotal,
  UsagePeriod,
  WorkPeriod,
  CacheAggregate,
} from './stats-types.js';
import type { Host, HostHold, HostsResponse } from './host-types.js';
import type { ProjectConsultation } from './project-types.js';
import type {
  PCToken,
  PermissionGrant,
  SSHTicket,
  CloseRequest,
  CloseMaterial,
  CloseReference,
  ModelOverrideView,
  ModelOverrideForm,
  ProviderDisableView,
  ProviderDisableForm,
} from './admin-types.js';
import type { ReloadRecord } from './ui-update.js';
interface ReloadState extends ReloadRecord {
  hash: string;
  screen?: { type: 'new'; share: boolean };
  sort?: { field: string; direction: string };
  kindFilter?: string;
}
import type {
  RequestRow,
  RequestGroups,
  ListData,
  ListScreen,
  ListEntityType,
  ListResponse,
  ListEntry,
  ListNotice,
  ListTarget,
  ListUnreadProject,
} from './list-types.js';
import type { TierRecord, TierChange, ModelOverride } from './tier-types.js';
import type {
  MarkdownList,
  MarkdownBlock,
  InlineToken,
} from './request-list-markdown.js';
import type {
  Draft,
  DraftMessage,
  DraftIssue,
  SplitPlan,
  ProxyEvidence,
  ReferenceNote,
} from './draft-types.js';
import type { Todo } from './todo-types.js';
import type { ComposerScope } from './composer-types.js';
import type {
  Issue,
  IssueScope,
  UIMessage,
  ImageDescriptor,
  PriorityRequest,
  IssueFullState,
} from './issue-types.js';
type ComposerController = ReturnType<typeof imageComposer>;
import type { DOMChild } from './dom.js';
import { uiError } from './models.js';
import type {
  SessionResponse,
  MetaResponse,
  CurrentScreen,
  Polling,
  APIProblem,
  UIError,
  Navigation,
  GiteaAccount,
  ParsedRoute,
  ListTab,
} from './models.js';
import { $, el, cancelTimer } from './dom.js';
import { requestJson } from './api-client.js';
import {
  shownProject as repositoryDisplay,
  shownRepositoryReference,
} from './repository-display.js';
import { deskRepositories } from './request-classification.js';
import { imageComposer, imageGallery } from './image-composer.js';
import {
  familyProgressText,
  familyProgressNote,
  requestGroupsWithAux,
  fullTestRows,
  attentionRows,
  initialTab,
  guidance,
  todoHeadline,
  sortRows,
  nextSort,
  defaultSort,
  kindFilters,
  defaultKindFilter,
  validKindFilter,
  filterRows,
  draftStatus,
  draftGuidance,
  closeReasons,
  issueProject,
  projectWaitText,
  workStatsNotes,
  workStatsRows,
  workDayHeaders,
  workDayCells,
  usageStatsNotes,
  usageHeaders,
  usageCells,
  usageTables,
  usageCorrectionText,
  tierOutcomeNotes,
  tierOutcomeHeaders,
  tierOutcomeCells,
  followupStatsNotes,
  followupStatsRows,
  followupRowHeaders,
  followupRowCells,
  shortTime,
  markdownBlocks,
  inlineTokens,
  longIdParts,
  projectStatusNames,
  projectGuidance,
  projectDefinitionRows,
  projectCreationRows,
  tierRows,
  overrideProviderNames,
  overrideScopeNote,
  overrideNoEffort,
  overrideRows,
  overrideDefaultRows,
  jstDeadline,
  projectFilingNames,
  projectIssueLabel,
  statusLabel,
  originLabel,
  investigationDraft,
  reportAuthor,
  handoffDraft,
} from './request-list.js';
import { updateWatcher } from './ui-update.js';
// 受付画面の入口。import・画面の状態（下の let）・起動（boot）だけを持つ。役割ごとのファイル（client/src/ui/app-*.js。
// Issue #456・#457、docs/components.md）へ分けた部分は、受付が /app.js を配信するときに、このファイルの最後の
// boot(); の前へ client/scripts/build.ts の順でつなぐ。全体で1つのモジュールになり、どのファイルも同じスコープの
// 状態・関数・import を共有する。
// 共通部品は client/src/ui/app-parts.js、入力欄の部品は client/src/ui/app-inputs.js、本文の簡易Markdownは client/src/ui/app-markdown.js、
// ルーティングは client/src/ui/app-routing.js、API呼び出し・通知は client/src/ui/app-api.js、定期取得と即時通知は client/src/ui/app-live.js、依頼一覧は client/src/ui/app-home.js、
// 新しい依頼は client/src/ui/app-new-request.js、新しいプロジェクトは client/src/ui/app-projects.js、相談（下書き）は client/src/ui/app-draft.js と
// client/src/ui/app-draft-view.js、TODOは client/src/ui/app-todo.js、Issueの詳細は client/src/ui/app-issue.js と client/src/ui/app-issue-view.js、
// クローズ依頼は client/src/ui/app-close-requests.js、稼働実績は client/src/ui/app-stats.js（実装の手戻りの内訳は
// client/src/ui/app-rework-stats.js）、モデルの切り替えは client/src/ui/app-models.js、
// 端末連携と接続状態は client/src/ui/app-device.js、画面の新版への自動更新と一覧の保持は client/src/ui/app-updates.js、実装の難易度の表示は
// client/src/ui/app-implementation-tiers.js にある。
let session: SessionResponse = { authenticated: false },
  meta: MetaResponse = { providers: [], projects: [] },
  current: CurrentScreen | null = null,
  tab: string | null = null,
  mobileView = 'chat',
  // スマホ幅でページを相談の末尾へ送った画面（routeId）。同じ画面の描き直しでは開いたときの扱いにしない。
  draftScrolled: number | null = null,
  issueKey: string | null = null,
  // Issue画面で「以前のやりとりを表示」を押した履歴タブ。同じIssueを見ている間は定期更新・タブ切替でも保つ。
  issueOlder = new Set(),
  timer: number | undefined,
  // 表示中の画面の定期取得（poll の予約。即時通知で前倒しする。liveNotice）。
  polling: Polling | null = null,
  routeId = 0,
  routeHash = location.hash,
  // 進行中・フォローアップタブの並び順（両タブ共通）。定期更新・タブ切替・詳細との往復・戻る/進むでも保ち、再読み込みで初期順に戻る
  // （画面の新版への自動更新では保つ）。
  issueSort = { ...defaultSort },
  // 「完了」「クローズ済み」タブの絞り込み（通常の依頼／フォローアップ）。並び順と同じ範囲で保つ。
  kindFilter = defaultKindFilter,
  // 実行中の保存・送信などの操作の数。0でない間は画面の自動更新を待たせる。
  operations = 0,
  // 表示中の画面が自動更新を待たせる理由（未保存の選択など）。画面ごとに登録し、reset() で外す。
  holdReason: (() => string | null) | null = null,
  // 表示中の依頼一覧（id は開いたときの routeId）。タブの切り替え・戻る/進むは、これが表示中なら取り直さず描き直す。
  listScreen: ListScreen | null = null,
  // 依頼一覧の最後の取得結果（4つのAPIの応答と取得時刻 at）。別の画面へ移っても持ち、一覧を開き直したときに先に描く。
  listCache: ListData | null = null;
async function boot() {
  if (reloadRecord === undefined) resumed = reloadRecord = takeReload();
  // 絞り込みは保存しない。再読み込みでは「すべて」に戻る（画面の新版への自動更新だけは restoreRoute が戻す）。
  kindFilter = defaultKindFilter;
  // 起動（再読み込み）では一覧を新しく開く。同じ版で保持した取得結果があれば先に描き、その後に取得する。
  listScreen = null;
  listCache = readList();
  try {
    const incoming = new URLSearchParams(location.hash.slice(1)).get('connect');
    if (incoming) {
      history.replaceState(null, '', location.pathname + location.search);
      await api('/api/desktop', { key: incoming });
    }
    session = await api('/api/session');
    if (!session.authenticated) {
      dropList();
      $('#connection').textContent = '接続コードを入力';
      connectionToggle('接続コードを入力', true);
      return pairing();
    }
    bootRoutingPending = true;
    const metaRequest = api('/api/meta');
    startBootReads();
    meta = await metaRequest;
    bootRoutingPending = false;
    const account = giteaAccountText(meta.giteaAccount);
    // 受付のPC名（設定の workerId）。返さない受付では「受付」。
    $('#connection').textContent =
      `${meta.workerId || '受付'} · ${meta.worker ? '稼働中' : '実行状態を確認'}` +
      (account ? ` · ${account.short}` : '');
    $('#connection').title = account ? account.full : '';
    $('#connection').classList.toggle('off', !meta.worker);
    connectionToggle(meta.worker ? '稼働中' : '実行状態を確認', !meta.worker);
    $('#phone').hidden = !session.local;
    $('#phone').onclick = () => phone().catch(error);
    // 即時通知は認証済みの画面だけが受ける（受付は画面のセッションだけに応じる）。隠れたタブは接続しない。
    watchLive();
    startLive();
    try {
      await openRoute();
    } finally {
      bootReads = null;
    }
  } catch (caughtValue) {
    bootRoutingPending = false;
    bootReads = null;
    const e = uiError(caughtValue);
    error(e);
    $('#app').replaceChildren(
      el('div', {
        class: 'empty',
        text: '受付に接続できませんでした。受付のPC（NASの受付ならそのコンテナ）で受付サービスが動いているか、開いているURLが受付のURLかを確認してください。',
      }),
    );
  } finally {
    // 起動用のPCリンクの受け取りと、最初の画面の表示が済んでから確認を始める。
    startUpdates();
  }
}
boot();
