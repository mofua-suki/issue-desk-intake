import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

test('SSH開発のHOME保護は固定検証の一時workspaceを覆わない', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'development-home-'));
  try {
    const bin = path.join(root, 'bin');
    fs.mkdirSync(bin);
    fs.writeFileSync(
      path.join(bin, 'docker'),
      `#!${process.execPath}
const args=process.argv.slice(2);
if(args[0]==='ps')console.log('development-container');
else if(args[0]==='inspect')console.log('existing-image');
else if(args[0]==='run'){
 const env={...process.env};
 for(let i=0;i<args.length;i++)if(args[i]==='-e'){const at=args[i+1].indexOf('=');env[args[i+1].slice(0,at)]=args[i+1].slice(at+1)}
 const result=require('node:child_process').spawnSync(process.execPath,['-e',\`const os=require('node:os'),fs=require('node:fs'),path=require('node:path');const work=fs.mkdtempSync(path.join(os.tmpdir(),'provider-work-'));try{const relative=path.relative(os.homedir(),work);if(relative===''||(!relative.startsWith('..'+path.sep)&&!path.isAbsolute(relative)))process.exitCode=31;}finally{fs.rmSync(work,{recursive:true,force:true})}\`],{env,stdio:'inherit'});
 process.exitCode=result.status ?? 1;
}
`,
      { mode: 0o700 },
    );
    const run = spawnSync(
      'sh',
      [
        fileURLToPath(
          new URL('../scripts/ssh-development-host.sh', import.meta.url),
        ),
      ],
      {
        env: {
          ...process.env,
          HOME: root,
          PATH: bin + path.delimiter + process.env.PATH,
        },
        encoding: 'utf8',
      },
    );
    assert.equal(run.status, 0, run.stderr);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
