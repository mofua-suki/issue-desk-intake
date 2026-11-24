export type DOMChild =
  | Node
  | string
  | number
  | boolean
  | null
  | undefined
  | DOMChild[];
type ElementEvent<E extends Event, T extends HTMLElement> = Omit<
  E,
  'currentTarget'
> & { currentTarget: T };
interface ElementProperties<T extends HTMLElement = HTMLElement> {
  [attribute: string]: unknown;
  class?: string;
  text?: unknown;
  onclick?: (event: ElementEvent<MouseEvent, T>) => unknown;
  onClick?: (event: ElementEvent<MouseEvent, T>) => unknown;
  oninput?: (event: ElementEvent<Event, T>) => unknown;
  onInput?: (event: ElementEvent<Event, T>) => unknown;
  onchange?: (event: ElementEvent<Event, T>) => unknown;
  onChange?: (event: ElementEvent<Event, T>) => unknown;
  onkeydown?: (event: ElementEvent<KeyboardEvent, T>) => unknown;
  onKeydown?: (event: ElementEvent<KeyboardEvent, T>) => unknown;
  onsubmit?: (event: ElementEvent<SubmitEvent, T>) => unknown;
  onSubmit?: (event: ElementEvent<SubmitEvent, T>) => unknown;
}

export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  props?: ElementProperties<HTMLElementTagNameMap[K]>,
  ...children: DOMChild[]
): HTMLElementTagNameMap[K];
export function el(
  tag: string,
  props?: ElementProperties,
  ...children: DOMChild[]
): HTMLElement;
export function el(
  tag: string,
  props: Record<string, unknown> = {},
  ...children: DOMChild[]
): HTMLElement {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (key.startsWith('on')) {
      if (value === null || value === undefined) continue;
      if (
        typeof value === 'object' &&
        'handleEvent' in value &&
        typeof value.handleEvent === 'function'
      )
        node.addEventListener(
          key.slice(2).toLowerCase(),
          value as EventListenerObject,
        );
      else if (typeof value === 'function')
        node.addEventListener(
          key.slice(2).toLowerCase(),
          value as EventListener,
        );
      else throw new TypeError('イベントの処理は関数で指定してください。');
    } else if (key === 'class') node.className = String(value);
    else if (key === 'text')
      node.textContent = value == null ? '' : String(value);
    else if (value !== undefined && value !== false)
      node.setAttribute(key, value === true ? '' : String(value));
  }
  for (const child of children.flat()) {
    if (child !== null && child !== undefined)
      node.append(
        typeof child === 'string'
          ? document.createTextNode(child)
          : (child as Node),
      );
  }
  return node;
}

interface PageElements {
  '#families': HTMLElement;
  '#app': HTMLElement;
  '#reply-input': HTMLTextAreaElement;
  '#close-reason': HTMLSelectElement;
  '#priority-choice': HTMLSelectElement;
  '#draft-priority-choice': HTMLSelectElement;
  '#handover-choice': HTMLSelectElement;
  '#phone-dialog': HTMLDialogElement;
  '#phone': HTMLButtonElement;
  '#connection-toggle': HTMLButtonElement;
  '#connection': HTMLElement;
  '#phone-content': HTMLElement;
  '#update': HTMLElement;
  '#tabs': HTMLElement;
  '#items': HTMLElement;
  '#hosts': HTMLElement;
  '#sorts': HTMLElement;
  '#kinds': HTMLElement;
  '#kind-summary': HTMLElement;
  '#notice': HTMLElement;
  '#toast': HTMLElement;
}

export const $ = <K extends keyof PageElements>(selector: K): PageElements[K] =>
  document.querySelector<PageElements[K]>(selector)!;

export function cancelTimer(timer: number | null | undefined): void {
  if (timer !== null) clearTimeout(timer);
}
