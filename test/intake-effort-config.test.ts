import { readFile } from 'node:fs/promises';
import assert from 'node:assert/strict';
import test from 'node:test';

test('shipped shared model settings retain unrelated tiers and use medium intake effort', async () => {
  const { models } = JSON.parse(
    await readFile(new URL('../assignments.json', import.meta.url), 'utf8'),
  );
  assert.deepEqual(models.implementation.tiers, {
    light: {
      model: 'gpt-6.1-terra',
      claudeModel: 'sonnet',
      reasoningEffort: 'medium',
    },
    standard: {
      model: 'gpt-6.1-terra',
      claudeModel: 'sonnet',
      reasoningEffort: 'medium',
    },
    heavy: {
      model: 'gpt-6.2-sol',
      claudeModel: 'opus',
      reasoningEffort: 'high',
    },
  });
  assert.equal(
    Object.hasOwn(models.intake.tiers, 'classifier'),
    false,
    '始めの判定は廃止した',
  );
  assert.deepEqual(models.intake.tiers.filingCheck, {
    model: 'gpt-6.1-terra',
    claudeModel: 'sonnet',
    reasoningEffort: 'medium',
  });
  assert.equal(models.intake.claudeModel, 'opus');
  assert.equal(models.intake.reasoningEffort, 'high');
  for (const tier of ['light', 'standard', 'heavy']) {
    assert.equal(models.intake.tiers[tier].reasoningEffort, 'medium', tier);
  }
});
