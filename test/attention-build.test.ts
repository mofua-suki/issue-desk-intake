import test from 'node:test';
import assert from 'node:assert/strict';
import {
  cpSync,
  mkdtempSync,
  mkdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { spawnSync } from 'node:child_process';

test('attention classification NAS client build', async () => {
  const root = fileURLToPath(new URL('../', import.meta.url));
  const fixture = mkdtempSync(join(tmpdir(), 'attention-nas-build-'));
  try {
    // Reproduce the shipped Docker stage's COPY inputs, rather than building
    // against the complete checkout where missing inputs would be hidden.
    const docker = readFileSync(join(root, 'deploy/nas/Dockerfile'), 'utf8');
    const stage = docker
      .split('FROM tools AS client-build\n')[1]
      .split('\nFROM ')[0];
    const client = join(fixture, 'client');
    mkdirSync(client, { recursive: true });
    for (const line of stage.split('\n')) {
      const copy = /^COPY --chown=node:node (.+) (\S+)$/.exec(line);
      if (!copy) continue;
      const destination =
        copy[2] === './'
          ? client
          : join(fixture, copy[2].replace(/^\/build\//, ''));
      mkdirSync(destination, { recursive: true });
      for (const source of copy[1].split(' ')) {
        const input = join(root, source);
        const target = source.endsWith('/')
          ? destination
          : join(destination, source.split('/').at(-1));
        cpSync(input, target, {
          recursive: true,
          filter: (path) =>
            !['node_modules', 'dist'].includes(path.split('/').at(-1)),
        });
      }
    }
    // Dependencies are installed before the source COPY in the actual stage.
    symlinkSync(
      resolve(root, 'client/node_modules'),
      join(client, 'node_modules'),
      'dir',
    );
    const typecheck = spawnSync(process.execPath, ['scripts/typecheck.ts'], {
      cwd: client,
      encoding: 'utf8',
    });
    assert.equal(
      typecheck.status,
      0,
      `${typecheck.stdout}\n${typecheck.stderr}`,
    );
    const sharedImport =
      "\nexport { draftStatus } from '../../../shared/src/desk-attention.mts';\n";
    writeFileSync(join(client, 'src/ui/another-attention.ts'), sharedImport);
    for (const name of [
      'src/ui/app-attention-alerts.ts',
      'src/api/client.ts',
    ]) {
      const file = join(client, name);
      writeFileSync(file, readFileSync(file, 'utf8') + sharedImport);
    }
    const build = spawnSync(process.execPath, ['scripts/build.ts'], {
      cwd: client,
      encoding: 'utf8',
    });
    assert.equal(build.status, 0, `${build.stdout}\n${build.stderr}`);
    const { ui } = JSON.parse(
      readFileSync(join(client, 'dist/ui-version.json'), 'utf8'),
    );
    assert.match(ui, /^[a-f0-9]{64}$/);
    assert.ok(
      readFileSync(join(client, 'dist/v', ui, 'desk-attention.js')).length > 0,
    );
    for (const name of ['another-attention.js', 'api-client.js']) {
      const module = await import(
        pathToFileURL(join(client, 'dist/v', ui, name)).href
      );
      assert.equal(
        module.draftStatus({ status: 'thinking', turnStopped: true }),
        'ai_stopped',
      );
    }
  } finally {
    rmSync(fixture, { recursive: true, force: true });
  }
});

test('client build rejects missing replacement and remaining shared imports', () => {
  const root = fileURLToPath(new URL('../', import.meta.url));
  const fixture = mkdtempSync(join(tmpdir(), 'attention-build-invalid-'));
  try {
    const client = join(fixture, 'client');
    for (const name of ['scripts', 'src', 'public']) {
      cpSync(join(root, 'client', name), join(client, name), {
        recursive: true,
      });
    }
    cpSync(join(root, 'client/package.json'), join(client, 'package.json'));
    cpSync(join(root, 'shared/src'), join(fixture, 'shared/src'), {
      recursive: true,
    });
    symlinkSync(
      resolve(root, 'client/node_modules'),
      join(client, 'node_modules'),
      'dir',
    );
    const classification = join(client, 'src/ui/request-classification.ts');
    const original = readFileSync(classification, 'utf8');
    const cases: [string, RegExp][] = [
      [
        original.replace(
          '../../../shared/src/desk-attention.mts',
          '../../shared/src/desk-attention.mts',
        ),
        /置換対象が見つかりません/,
      ],
      [
        original + "\nexport * from '../../shared/src/other.mjs';\n",
        /shared への相対パスが出力に残っています/,
      ],
    ];
    for (const [input, error] of cases) {
      writeFileSync(classification, input);
      const result = spawnSync(process.execPath, ['scripts/build.ts'], {
        cwd: client,
        encoding: 'utf8',
      });
      assert.notEqual(result.status, 0, result.stdout);
      assert.match(result.stderr, error);
    }
  } finally {
    rmSync(fixture, { recursive: true, force: true });
  }
});
