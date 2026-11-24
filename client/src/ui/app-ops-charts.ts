// 運用の概要で使う汎用のSVGグラフ部品（横棒・縦棒・ドーナツ・折れ線・凡例・タブ・枠）。app.js と同じスコープで、
// client/scripts/build.ts の順で app-ops-overview.js の直前につなぐ。
const opsColors = [
  'ops-color-0',
  'ops-color-1',
  'ops-color-2',
  'ops-color-3',
  'ops-color-4',
  'ops-color-5',
];
function opsSvgNode(
  tag: string,
  attributes: Record<string, unknown> = {},
  ...children: (string | Node)[]
) {
  const node = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [key, value] of Object.entries(attributes))
    node.setAttribute(key, String(value));
  node.append(...children);
  return node;
}
function opsSvg(title: string, height = 32) {
  const svg = opsSvgNode('svg', {
    viewBox: `0 0 400 ${height}`,
    class: 'ops-chart',
    role: 'img',
    'aria-label': title,
  });
  const label = opsSvgNode('title');
  label.textContent = title;
  svg.append(label);
  return svg;
}
// 横棒は実数の最大値、100%棒はAPIの百分率をそのまま使う。欠測は図形にしない。
function opsBars<T extends { label: string }, K extends keyof T>(
  title: string,
  rows: T[],
  keys: K[],
  percent = false,
) {
  const maximum = percent
    ? 100
    : Math.max(
        1,
        ...rows.map((row) =>
          keys.reduce((sum, key) => {
            const value = row[key];
            return sum + (typeof value === 'number' ? value : 0);
          }, 0),
        ),
      );
  return rows.map((row) => {
    const svg = opsSvg(`${title}: ${row.label}`);
    let x = 0;
    keys.forEach((key, index: number) => {
      const value = row[key];
      if (typeof value !== 'number') return;
      const width = (value / maximum) * 400;
      svg.append(
        opsSvgNode('rect', {
          x,
          y: 4,
          width,
          height: 24,
          class: opsColors[index % opsColors.length],
          'data-value': value,
        }),
      );
      x += width;
    });
    return el(
      'div',
      { class: 'ops-bar-row' },
      el('p', { text: row.label }),
      svg,
    );
  });
}
function opsColumns(title: string, rows: { value: number | null }[]) {
  const svg = opsSvg(title, 180);
  const maximum = Math.max(1, ...rows.map((row) => row.value ?? 0));
  const step = 400 / Math.max(1, rows.length);
  rows.forEach((row: { value: number | null }, index: number) => {
    if (row.value == null) return;
    const height = (row.value / maximum) * 160;
    svg.append(
      opsSvgNode('rect', {
        x: index * step + step * 0.15,
        y: 170 - height,
        width: step * 0.7,
        height,
        class: opsColors[index % opsColors.length],
        'data-value': row.value,
      }),
    );
  });
  return svg;
}
function opsDonut(steps: { percent: number | null }[]) {
  const svg = opsSvg('段階ごとのトークンの割合', 220);
  let offset = 0;
  steps.forEach((row: { percent: number | null }, index: number) => {
    if (row.percent == null || row.percent <= 0) return;
    svg.append(
      opsSvgNode('circle', {
        cx: 200,
        cy: 110,
        r: 75,
        pathLength: 100,
        'stroke-dasharray': `${row.percent} ${100 - row.percent}`,
        'stroke-dashoffset': -offset,
        class: `ops-donut ${opsColors[index % opsColors.length]}`,
        'data-value': row.percent,
      }),
    );
    offset += row.percent;
  });
  return svg;
}
function opsLines<T extends { date: string }, K extends keyof T>(
  title: string,
  rows: T[],
  keys: K[],
) {
  const svg = opsSvg(title, 180);
  const maximum = Math.max(
    1,
    ...rows.flatMap((row) =>
      keys.map((key) => {
        const value = row[key];
        return typeof value === 'number' ? value : 0;
      }),
    ),
  );
  keys.forEach((key, index: number) => {
    let points: string[] = [];
    const flush = () => {
      if (points.length > 1)
        svg.append(
          opsSvgNode('polyline', {
            points: points.join(' '),
            class: `ops-line ${opsColors[index % opsColors.length]}`,
            'data-series': key,
          }),
        );
      points = [];
    };
    rows.forEach((row, day: number) => {
      const value = row[key];
      if (typeof value !== 'number') {
        flush();
        return;
      }
      const x = 8 + (day / Math.max(1, rows.length - 1)) * 384;
      const y = 170 - (value / maximum) * 160;
      points.push(`${x},${y}`);
      svg.append(
        opsSvgNode('circle', {
          cx: x,
          cy: y,
          r: 3,
          class: opsColors[index % opsColors.length],
          'data-series': key,
          'data-date': row.date,
          'data-value': value,
        }),
      );
    });
    flush();
  });
  return svg;
}
function opsLegend(labels: DOMChild[]) {
  return el(
    'ul',
    { class: 'ops-legend' },
    ...labels.map(
      (
        label: string | number | boolean | Node | DOMChild[] | null | undefined,
        index: number,
      ) =>
        el(
          'li',
          {},
          el('span', {
            class: `ops-swatch ${opsColors[index % opsColors.length]}`,
            'aria-hidden': 'true',
          }),
          label,
        ),
    ),
  );
}
// 同じ枠の中の表を切り替えるタブ。隠したパネルもDOMに残し、選んだものだけ表示する。
function opsTabs(
  label: string,
  panels: { title: string; content: (HTMLElement | SVGElement)[] }[],
) {
  const id = `ops-tabs-${++opsTabsCount}`;
  const buttons: HTMLButtonElement[] = [];
  const bodies: HTMLElement[] = [];
  const select = (index: number, focus = false) => {
    buttons.forEach((button, i) => {
      const active = i === index;
      button.className = active ? 'tab active' : 'tab';
      button.setAttribute('aria-selected', String(active));
      button.tabIndex = active ? 0 : -1;
      bodies[i].hidden = !active;
    });
    if (focus) buttons[index].focus();
  };
  panels.forEach((panel, index) => {
    buttons.push(
      el('button', {
        type: 'button',
        role: 'tab',
        id: `${id}-tab-${index}`,
        'aria-controls': `${id}-panel-${index}`,
        text: panel.title,
        onclick: () => select(index),
        onkeydown: (event) => {
          const step =
            event.key === 'ArrowRight' ? 1 : event.key === 'ArrowLeft' ? -1 : 0;
          if (!step) return;
          event.preventDefault();
          select((index + step + panels.length) % panels.length, true);
        },
      }),
    );
    bodies.push(
      el(
        'div',
        {
          role: 'tabpanel',
          id: `${id}-panel-${index}`,
          'aria-labelledby': `${id}-tab-${index}`,
        },
        ...panel.content,
      ),
    );
  });
  select(0);
  return el(
    'div',
    { class: 'ops-tabs' },
    el(
      'div',
      { class: 'ops-tab-list', role: 'tablist', 'aria-label': label },
      ...buttons,
    ),
    ...bodies,
  );
}
let opsTabsCount = 0;
function opsSection(
  title: string,
  definition: string,
  ...content: (HTMLElement | SVGElement)[]
) {
  return el(
    'section',
    { class: 'ops-panel' },
    el('h3', { text: title }),
    ...content,
    el('p', { class: 'ops-definition', text: `定義: ${definition}` }),
  );
}
