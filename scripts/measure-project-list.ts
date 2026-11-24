#!/usr/bin/env node
import { performance } from 'node:perf_hooks';
import { pathToFileURL } from 'node:url';

// Issue a list and a numberless single-project GET together. Only timings and
// statuses are retained; neither authorization headers nor response bodies are logged.
export async function measure(base, projectID, samples, cookie = '') {
  const origin = new URL(base);
  if (
    !['http:', 'https:'].includes(origin.protocol) ||
    origin.username ||
    origin.password ||
    origin.pathname !== '/' ||
    origin.search ||
    origin.hash
  )
    throw new Error('base must be an HTTP(S) origin');
  if (
    !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
      projectID,
    )
  )
    throw new Error('invalid project ID');
  if (!Number.isSafeInteger(samples) || samples < 1)
    throw new Error('samples must be a positive integer');
  const results = [];
  async function request(path) {
    const start = performance.now();
    const response = await fetch(new URL(path, origin), {
      headers: cookie ? { cookie } : {},
      redirect: 'manual',
    });
    await response.arrayBuffer();
    if (response.status !== 200)
      throw new Error(
        `${path === '/api/projects' ? 'list' : 'project'} returned HTTP ${response.status}`,
      );
    return performance.now() - start;
  }
  for (let i = 0; i < samples; i++) {
    const [listMs, projectMs] = await Promise.all([
      request('/api/projects'),
      request(`/api/projects/${projectID}`),
    ]);
    results.push({ listMs, projectMs });
  }
  return results;
}

function percentile(values, p) {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.ceil(sorted.length * p) - 1];
}
export function summarize(results) {
  return Object.fromEntries(
    ['listMs', 'projectMs'].map((key) => [
      key,
      {
        p50: percentile(
          results.map((r) => r[key]),
          0.5,
        ),
        p95: percentile(
          results.map((r) => r[key]),
          0.95,
        ),
      },
    ]),
  );
}

async function main() {
  const [base, id, count, ...extra] = process.argv.slice(2);
  if (!base || !id || !count || extra.length || !/^\d+$/.test(count))
    throw new Error(
      'usage: node scripts/measure-project-list.ts ORIGIN PROJECT_ID SAMPLES',
    );
  const results = await measure(
    base,
    id,
    Number(count),
    process.env.ISSUE_DESK_MEASURE_COOKIE || '',
  );
  console.log(
    JSON.stringify(
      { samples: results.length, summary: summarize(results), results },
      null,
      2,
    ),
  );
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href)
  main().catch((e) => {
    console.error(e.message);
    process.exitCode = 1;
  });
