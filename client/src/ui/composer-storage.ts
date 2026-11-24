import type { ComposerState } from './composer-types.js';

let database: Promise<IDBDatabase> | undefined;
const writes = new Map<string, Promise<unknown>>();

function db(): Promise<IDBDatabase> {
  return (database ??= new Promise<IDBDatabase>((resolve, reject) => {
    const request = indexedDB.open('issue-desk-composers', 1);
    request.onupgradeneeded = () =>
      request.result.createObjectStore('messages');
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  }));
}

async function storage(
  key: string,
  value?: ComposerState | null,
  expected?: string | Set<string>,
): Promise<unknown> {
  const database = await db();
  return new Promise((resolve, reject) => {
    const transaction = database.transaction(
      'messages',
      value === undefined ? 'readonly' : 'readwrite',
    );
    const store = transaction.objectStore('messages');
    let removed = false;
    const request = expected
      ? store.get(key)
      : value === undefined
        ? store.get(key)
        : value === null
          ? store.delete(key)
          : store.put(value, key);
    if (expected)
      request.onsuccess = () => {
        const record = request.result as ComposerState | undefined;
        if (
          expected instanceof Set
            ? record?.version !== undefined && expected.has(record.version)
            : record?.version === expected
        ) {
          store.delete(key);
          removed = true;
        }
      };
    transaction.oncomplete = () => resolve(expected ? removed : request.result);
    transaction.onerror = () => reject(transaction.error);
    transaction.onabort = () => reject(transaction.error);
  });
}

export function stored(key: string): Promise<ComposerState | undefined>;
export function stored(
  key: string,
  value: null,
  expected: string | Set<string>,
): Promise<boolean>;
export function stored(key: string, value: ComposerState): Promise<unknown>;
export function stored(
  key: string,
  value?: ComposerState | null,
  expected?: string | Set<string>,
): Promise<unknown> {
  const task = (writes.get(key) ?? Promise.resolve()).then(() =>
    storage(key, value, expected),
  );
  writes.set(
    key,
    task.catch(() => {}),
  );
  return task;
}
