import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';

const root = path.resolve(import.meta.dirname, '..');

// NASの利用者ユニット（update-units.sh が __DIR__ を配置先へ置き換えて配る）の起動行が、配置先に実在するファイルを指すこと。
// 入口の改名などで起動行を直し忘れると、タイマーが毎回失敗して許可の反映・更新・控えが止まる。
test('NASのユニットの起動行は、配置先に実在するスクリプトを指す', () => {
  const dir = path.join(root, 'deploy/nas');
  const units = fs.readdirSync(dir).filter((name) => name.endsWith('.service'));
  let checked = 0;
  for (const unit of units) {
    for (const line of fs
      .readFileSync(path.join(dir, unit), 'utf8')
      .split('\n')) {
      if (!line.startsWith('ExecStart=')) continue;
      for (const word of line.slice('ExecStart='.length).split(/\s+/)) {
        if (!word.startsWith('__DIR__/')) continue;
        assert.ok(
          fs.existsSync(path.join(root, word.slice('__DIR__/'.length))),
          `${unit}: ${word}`,
        );
        checked += 1;
      }
    }
  }
  assert.ok(
    checked >= 3,
    `配置先のスクリプトを指す起動行が見つかりません（${checked}件）`,
  );
});
