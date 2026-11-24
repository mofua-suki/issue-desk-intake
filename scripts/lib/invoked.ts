// 手書きのスクリプトが直接実行されたか（import されただけでないか）を判定する。
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * moduleUrl（呼び出し側の import.meta.url）のファイルが entry（既定は node に渡された入口 process.argv[1]）か。
 * どちらも実体のパス（リンクを解決した値）で比べ、解決できなければ false。
 * @param [options] ignoreCase: 大文字小文字を区別せずに比べる
 */
export function invokedAs(
  moduleUrl: string,
  entry: string | undefined = process.argv[1],
  { ignoreCase = false }: { ignoreCase?: boolean } = {},
): boolean {
  if (!entry) return false;
  try {
    const file = fs.realpathSync.native(path.resolve(entry));
    const self = fs.realpathSync.native(fileURLToPath(moduleUrl));
    return ignoreCase
      ? file.toLowerCase() === self.toLowerCase()
      : file === self;
  } catch {
    return false;
  }
}
