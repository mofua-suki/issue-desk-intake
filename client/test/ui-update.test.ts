import test from 'node:test';
import assert from 'node:assert/strict';
import {
  updateWatcher,
  updateInterval,
  holdInterval,
  reloadGuardMs,
  checkTimeout,
  updateMessages,
  validVersion,
} from '../src/ui/ui-update.ts';

const A = 'a'.repeat(64),
  B = 'b'.repeat(64),
  C = 'c'.repeat(64);
// 時間は進めず、予約されたタイマーを検証側が呼ぶ。
// baseline を undefined と明示したときは既定値に置き換えない（版を持たない画面の検証に使う）。
function harness(
  options: { last?: any; served?: any; baseline?: string } = {},
) {
  const { last = null, served = { ready: true, ui: A } } = options;
  const baseline = 'baseline' in options ? options.baseline : A;
  const h: Record<string, any> = {
    served,
    reason: null,
    visible: true,
    healthCalls: 0,
    reloads: 0,
    prepared: [],
    shown: [],
    timers: new Map(),
    nextTimer: 0,
    time: 1_000_000,
    failPrepare: false,
    gate: null,
    signals: [],
  };
  h.watcher = updateWatcher({
    baseline,
    last,
    async health(options) {
      h.healthCalls++;
      h.signals.push(options?.signal);
      if (h.gate) await h.gate;
      if (h.served instanceof Error) throw h.served;
      return h.served;
    },
    hold: () => h.reason,
    prepare(record) {
      if (h.failPrepare) throw Error('storage blocked');
      h.prepared.push(record);
    },
    reload: () => h.reloads++,
    show: (message) => h.shown.push(message),
    schedule(fn, ms) {
      h.timers.set(++h.nextTimer, { fn, ms });
      return h.nextTimer;
    },
    cancel: (id) => h.timers.delete(id),
    visible: () => h.visible,
    now: () => h.time,
  });
  // 残っている予約は常に1本だけのはず。
  h.pending = () => {
    assert.ok(h.timers.size <= 1, `予約は1本まで: ${h.timers.size}`);
    return [...h.timers.values()][0];
  };
  h.fire = async () => {
    const [id, timer] = [...h.timers][0];
    h.timers.delete(id);
    await timer.fn();
    for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
  };
  // 待ち時間で見分けた予約を呼ぶ（確認の期限など、定期の予約と並んで残るもの）。
  h.fireMs = async (ms) => {
    const [id, timer] = [...h.timers].find(([, t]) => t.ms === ms);
    h.timers.delete(id);
    await timer.fn();
    for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
  };
  return h;
}

test('version strings are accepted only as 64 lowercase hex characters', () => {
  assert.equal(validVersion(A), true);
  for (const v of [
    undefined,
    null,
    '',
    'A'.repeat(64),
    'a'.repeat(63),
    'g'.repeat(64),
    1,
    {},
  ])
    assert.equal(validVersion(v), false);
});

test('the same version never reloads and keeps checking every 15 seconds', async () => {
  const h = harness();
  await h.watcher.start();
  assert.equal(h.healthCalls, 1, '読み込み直後に1回確かめる');
  assert.equal(h.reloads, 0);
  assert.equal(h.pending().ms, updateInterval);
  assert.equal(updateInterval, 15000);
  for (let i = 0; i < 3; i++) await h.fire();
  assert.equal(h.healthCalls, 4);
  assert.equal(h.reloads, 0);
  assert.deepEqual(h.shown.filter(Boolean), [], '知らせは出さない');
});

test('a new version on a ready service reloads once, recording where it came from', async () => {
  const h = harness();
  await h.watcher.start();
  h.served = { ready: true, ui: B, revision: 'x' };
  await h.fire();
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1);
  assert.deepEqual(h.prepared, [{ from: A, to: B, at: h.time }]);
  assert.equal(h.timers.size, 0, '再読み込みを始めたら次の確認を予約しない');
  await h.watcher.check();
  await h.watcher.wake();
  assert.equal(h.reloads, 1, '同じ画面から2回目の再読み込みをしない');
  assert.equal(h.healthCalls, 2);
  assert.equal(h.watcher.state().reloading, true);
});

test('an update deployed between page load and the first check is not mistaken for the loaded page', async () => {
  // 画面は A で読み込まれ、最初の確認までに B が配信され始めた。
  const h = harness({ served: { ready: true, ui: B } });
  await h.watcher.start();
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1);
  assert.equal(h.prepared[0].to, B);
});

test('failures, restarts, ready=false and missing or malformed versions keep the page and retry later', async () => {
  const h = harness();
  await h.watcher.start();
  const responses: any[] = [
    Error('network down'),
    { ready: false, ui: B },
    { ready: true },
    { ready: true, ui: null },
    { ready: true, ui: 'B'.repeat(64) },
    { ready: true, ui: 'b'.repeat(10) },
    { ready: 'yes', ui: B },
    null,
    [],
  ];
  for (const served of responses) {
    h.served = served;
    await h.fire();
    assert.equal(h.reloads, 0, JSON.stringify(served?.message ?? served));
    assert.equal(h.pending().ms, updateInterval, '後で確かめ直す');
    assert.equal(h.shown.at(-1), null);
  }
  // 回復すれば、取り逃した変更をそのまま反映する。
  h.served = { ready: true, ui: B };
  await h.fire();
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1);
});

test('a pending update is dropped when the service stops answering before it is safe', async () => {
  const h = harness();
  await h.watcher.start();
  h.reason = '入力中';
  h.served = { ready: true, ui: B };
  await h.fire();
  assert.equal(h.reloads, 0);
  assert.equal(h.shown.at(-1), updateMessages.hold('入力中'));
  assert.equal(h.pending().ms, holdInterval);
  // 保留中の見回りでは通信しない。
  const calls = h.healthCalls;
  await h.fire();
  assert.equal(h.healthCalls, calls);
  assert.equal(h.pending().ms, holdInterval);
  // 安全になった時点で確かめ直し、再起動中なら読み込み直さない。
  h.reason = null;
  h.served = { ready: false, ui: B };
  await h.fire();
  assert.equal(h.healthCalls, calls + 1);
  assert.equal(h.reloads, 0);
  assert.equal(h.shown.at(-1), null, '保留の知らせを消す');
  assert.equal(h.pending().ms, updateInterval);
  h.served = { ready: true, ui: B };
  await h.fire();
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1);
});

test('a held update offers the button once the page is safe again, without re-sending anything', async () => {
  const h = harness();
  await h.watcher.start();
  h.reason = '保存・送信の処理中です';
  h.served = { ready: true, ui: B };
  await h.fire();
  assert.match(
    h.shown.at(-1),
    /^更新があります。入力・送信が終わると「今すぐ更新」で読み込み直せます（保存・送信の処理中です）。$/,
  );
  // 復帰イベントが来ても、保留中は読み込み直さない。
  await h.watcher.wake();
  assert.equal(h.reloads, 0);
  assert.equal(h.pending().ms, holdInterval);
  h.reason = null;
  await h.fire();
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1);
  assert.equal(h.prepared.length, 1);
});

test('checks never overlap: timers, tab return and focus share the one in flight', async () => {
  const h = harness();
  let open;
  h.gate = new Promise((r) => (open = r));
  const first = h.watcher.start();
  const again = [
    h.watcher.check(),
    h.watcher.wake(),
    h.watcher.wake(),
    h.watcher.check(),
  ];
  assert.equal(h.healthCalls, 1);
  assert.equal(h.watcher.state().checking, true);
  h.served = { ready: true, ui: B };
  open();
  await Promise.all([first, ...again]);
  assert.equal(h.healthCalls, 1);
  assert.equal(h.reloads, 0, '表示中は案内だけ');
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1, '要求で1回だけ読み込み直す');
});

test('a hidden tab waits, and returning to it checks at once', async () => {
  const h = harness();
  await h.watcher.start();
  h.visible = false;
  h.served = { ready: true, ui: B };
  await h.fire();
  await h.watcher.wake();
  assert.equal(h.healthCalls, 1, '非表示の間は通信しない');
  assert.equal(h.pending().ms, updateInterval);
  h.visible = true;
  await h.watcher.wake();
  assert.equal(h.healthCalls, 2);
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1);
});

test('when the screen cannot be recorded, the update is held with an explanation', async () => {
  const h = harness();
  h.failPrepare = true;
  h.served = { ready: true, ui: B };
  await h.watcher.start();
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 0);
  assert.equal(h.shown.at(-1), updateMessages.storage);
  assert.equal(h.pending().ms, updateInterval);
  h.failPrepare = false;
  await h.fire();
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1);
});

test('a reload that came back to the same page is not repeated for that version', async () => {
  // 直前に A から B へ読み込み直したのに、また A の画面が返ってきた。
  const h = harness({
    last: { from: A, to: B, at: 1_000_000 - 1000 },
    served: { ready: true, ui: B },
  });
  await h.watcher.start();
  assert.equal(h.reloads, 0, 'リロードループにしない');
  assert.equal(h.shown.at(-1), updateMessages.stuck);
  assert.equal(h.pending().ms, updateInterval);
  await h.fire();
  assert.equal(h.reloads, 0);
  // さらに新しい版なら読み込み直す。
  h.served = { ready: true, ui: C };
  await h.fire();
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1);
  // 記録が古い・別の画面からのもの・壊れているときは、普段どおり読み込み直す。
  for (const last of [
    { from: A, to: B, at: 1_000_000 - reloadGuardMs },
    { from: C, to: B, at: 1_000_000 - 1000 },
    { from: A, to: 'bad', at: 1_000_000 - 1000 },
    { from: A, to: B, at: 1_000_000 + 1000 },
  ]) {
    const other = harness({ last, served: { ready: true, ui: B } });
    await other.watcher.start();
    other.visible = false;
    await other.watcher.hidden();
    assert.equal(other.reloads, 1, JSON.stringify(last));
  }
});

test('the guard against repeating a failed reload ends after 5 minutes on the page that stays open', async () => {
  const h = harness({
    last: { from: A, to: B, at: 1_000_000 - 1000 },
    served: { ready: true, ui: B },
  });
  await h.watcher.start();
  assert.equal(h.reloads, 0);
  assert.equal(h.watcher.state().stuck, B);
  assert.equal(h.shown.at(-1), updateMessages.stuck);
  // 期限の直前はまだ読み込み直さない。
  h.time = 1_000_000 - 1000 + reloadGuardMs - 1;
  await h.fire();
  assert.equal(h.reloads, 0);
  assert.equal(h.shown.at(-1), updateMessages.stuck);
  // 記録から5分たてば、起動し直さなくても同じ版へ読み込み直す。
  h.time = 1_000_000 - 1000 + reloadGuardMs;
  assert.equal(h.watcher.state().stuck, null);
  h.visible = false;
  await h.watcher.hidden();
  assert.equal(h.reloads, 1);
  assert.deepEqual(h.prepared, [{ from: A, to: B, at: h.time }]);
});

test('a check whose answer never arrives is aborted at the deadline and checking resumes', async () => {
  const h = harness();
  await h.watcher.start();
  assert.equal(checkTimeout, 10000);
  assert.ok(checkTimeout < updateInterval);
  // 応答が返らない。
  let late;
  h.gate = new Promise((r) => (late = r));
  h.served = { ready: true, ui: B };
  const stalled = h.watcher.check();
  assert.equal(h.watcher.state().checking, true);
  assert.equal(h.signals.at(-1).aborted, false);
  // 復帰イベントは同じ確認にまとまり、通信を増やさない。
  h.watcher.wake();
  assert.equal(h.healthCalls, 2);
  await h.fireMs(checkTimeout);
  await stalled;
  assert.equal(h.signals.at(-1).aborted, true, '期限で通信を中断する');
  assert.equal(h.watcher.state().checking, false);
  assert.equal(h.reloads, 0, '期限切れは失敗として画面をそのままにする');
  assert.equal(h.shown.at(-1), null);
  assert.equal(h.pending().ms, updateInterval, '後で確かめ直す');
  // 遅れて届いた応答は使わない。
  late();
  for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
  assert.equal(h.reloads, 0);
  // 復帰イベントで新しい確認を始められる。
  h.gate = null;
  await h.watcher.wake();
  assert.equal(h.healthCalls, 3);
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1);
});

test('a finished check cancels its deadline, and a missing AbortController still times out', async () => {
  const h = harness();
  await h.watcher.start();
  assert.deepEqual(
    [...h.timers.values()].map((t) => t.ms),
    [updateInterval],
    '期限の予約は残らない',
  );
  const saved = globalThis.AbortController;
  try {
    globalThis.AbortController = undefined;
    h.gate = new Promise(() => {});
    const stalled = h.watcher.check();
    assert.equal(
      h.signals.at(-1),
      undefined,
      '中断できない環境では signal を渡さない',
    );
    await h.fireMs(checkTimeout);
    await stalled;
    assert.equal(h.watcher.state().checking, false);
    assert.equal(h.pending().ms, updateInterval);
  } finally {
    globalThis.AbortController = saved;
  }
});

test('a page without a valid baseline never checks or reloads', async () => {
  for (const baseline of [undefined, '', 'not-a-version']) {
    const h = harness({ baseline, served: { ready: true, ui: B } });
    assert.equal(h.watcher.enabled, false);
    await h.watcher.start();
    await h.watcher.wake();
    assert.equal(h.healthCalls, 0);
    assert.equal(h.reloads, 0);
    assert.equal(h.timers.size, 0);
  }
});

test('visible initial, periodic and wake checks only offer the explicit update, even after a held click', async () => {
  const h = harness({ served: { ready: true, ui: B } });
  await h.watcher.start();
  await h.fire();
  await h.watcher.wake();
  assert.equal(h.reloads, 0);
  assert.equal(h.shown.at(-1), updateMessages.available);
  h.reason = '入力中';
  h.watcher.requestUpdate();
  assert.equal(h.shown.at(-1), updateMessages.hold('入力中'));
  assert.equal(h.reloads, 0);
  h.reason = null;
  await h.fire();
  assert.equal(h.reloads, 0, '保留中の要求は持ち越さない');
  assert.equal(h.shown.at(-1), updateMessages.available);
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1);
});

test('leaving the screen rechecks a known update, and hidden holds can release in the background', async () => {
  for (const kind of ['hidden', 'navigate']) {
    const h = harness({ served: { ready: true, ui: B } });
    await h.watcher.start();
    if (kind === 'hidden') h.visible = false;
    await h.watcher[kind]();
    assert.equal(h.healthCalls, 2, kind);
    assert.equal(h.reloads, 1, kind);
  }
  const h = harness({ served: { ready: true, ui: B } });
  await h.watcher.start();
  h.reason = '送信中';
  h.visible = false;
  await h.watcher.hidden();
  assert.equal(h.reloads, 0);
  await h.fire();
  assert.equal(h.healthCalls, 2, '保留中は通信しない');
  h.reason = null;
  await h.fire();
  assert.equal(h.healthCalls, 3);
  assert.equal(h.reloads, 1);
});

test('returning before a hidden check completes or a hold releases cancels automatic updating', async () => {
  const h = harness({ served: { ready: true, ui: B } });
  await h.watcher.start();
  let release;
  h.gate = new Promise((r) => (release = r));
  h.visible = false;
  const leaving = h.watcher.hidden();
  h.visible = true;
  const waking = h.watcher.wake();
  release();
  await Promise.all([leaving, waking]);
  assert.equal(h.reloads, 0);
  assert.equal(h.shown.at(-1), updateMessages.available);
  h.gate = null;
  h.reason = '入力中';
  h.visible = false;
  await h.watcher.hidden();
  h.visible = true;
  h.reason = null;
  await h.fire();
  assert.equal(h.reloads, 0);
});

test('automatic events require a known ready update, respect the guard, and discard navigation holds', async () => {
  const h = harness();
  await h.watcher.start();
  h.visible = false;
  await h.watcher.hidden();
  await h.watcher.navigate();
  assert.equal(h.healthCalls, 1, '新版未確認なら再確認しない');
  h.visible = true;
  h.served = { ready: true, ui: B };
  await h.watcher.wake();
  for (const served of [
    { ready: false, ui: B },
    { ready: true, ui: A },
    Error('offline'),
  ]) {
    h.served = served;
    await h.watcher.navigate();
    assert.equal(h.reloads, 0);
    h.served = { ready: true, ui: B };
    await h.watcher.check();
  }
  h.reason = '入力中';
  await h.watcher.navigate();
  h.reason = null;
  await h.fire();
  assert.equal(h.reloads, 0, '画面移動の要求は保留後に持ち越さない');
  const guarded = harness({
    last: { from: A, to: B, at: 999000 },
    served: { ready: true, ui: B },
  });
  await guarded.watcher.start();
  await guarded.watcher.navigate();
  guarded.visible = false;
  await guarded.watcher.hidden();
  assert.equal(guarded.reloads, 0);
  guarded.watcher.requestUpdate();
  assert.equal(guarded.reloads, 1, '明示要求は5分の繰り返し防止を無視する');
});

test('leaving during an existing check waits and rechecks once before updating', async () => {
  const h = harness({ served: { ready: true, ui: B } });
  await h.watcher.start();
  let release;
  h.gate = new Promise((r) => (release = r));
  const checking = h.watcher.check();
  h.visible = false;
  const leaving = h.watcher.hidden();
  assert.equal(h.healthCalls, 2);
  release();
  await Promise.all([checking, leaving]);
  assert.equal(h.healthCalls, 3);
  assert.equal(h.reloads, 1);
  await h.watcher.hidden();
  await h.watcher.navigate();
  h.watcher.requestUpdate();
  assert.equal(h.reloads, 1, '再読み込み開始後の要求は無視する');
  const disabled = harness({ baseline: '' });
  await disabled.watcher.hidden();
  await disabled.watcher.navigate();
  disabled.watcher.requestUpdate();
  assert.equal(disabled.healthCalls, 0);
});
