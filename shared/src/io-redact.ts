// 表示する文字列の伏せ字処理。通知を分ける前に適用する。
const secrets = { token: '', encoded: '', intake: '', worker: '', webhook: '' };
// 伏せ字だけにして短縮しない。長い通知を分けるときは、分ける前にこれを通す（分かれ目で秘密が伏せられずに残らない）。
export const redactAll = (text: unknown): string =>
  String(text)
    .replaceAll(secrets.token || '\0', '[REDACTED]')
    .replaceAll(secrets.intake || '\0', '[REDACTED]')
    .replaceAll(secrets.worker || '\0', '[REDACTED]')
    .replaceAll(secrets.encoded || '\0', '[REDACTED]')
    .replaceAll(secrets.webhook || '\0', '[REDACTED]')
    .replace(/(Authorization:\s*\S+\s+)\S+/gi, '$1[REDACTED]');
