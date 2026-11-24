/* global module */
'use strict';

// Inputs are non-secret identifiers and metadata supplied by a trusted issuer.
function canPublish({
  registration,
  candidate,
  credential,
  now = Date.now(),
} = {}) {
  const repoPattern = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})\/[A-Za-z0-9_.-]+$/;
  const refPattern = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;
  if (
    !registration ||
    !candidate ||
    !credential ||
    typeof registration.repository !== 'string' ||
    !repoPattern.test(registration.repository) ||
    registration.repository.split('/')[1] === '.' ||
    registration.repository.split('/')[1] === '..' ||
    typeof registration.credentialRef !== 'string' ||
    !refPattern.test(registration.credentialRef) ||
    candidate.repository !== registration.repository ||
    candidate.credentialRef !== registration.credentialRef ||
    credential.repository !== registration.repository ||
    credential.credentialRef !== registration.credentialRef ||
    typeof candidate.changesWorkflow !== 'boolean' ||
    !Number.isFinite(now)
  )
    return false;

  if (credential.expiresAt !== undefined) {
    if (
      typeof credential.expiresAt !== 'string' ||
      !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{3})?Z$/.test(
        credential.expiresAt,
      )
    )
      return false;
    const expiry = Date.parse(credential.expiresAt);
    if (
      !Number.isFinite(expiry) ||
      expiry <= now ||
      new Date(expiry).toISOString() !==
        credential.expiresAt.replace(
          /Z$/,
          credential.expiresAt.includes('.') ? 'Z' : '.000Z',
        )
    )
      return false;
  }
  const permissions = credential.permissions;
  return (
    !!permissions &&
    permissions.contents === 'write' &&
    permissions.pull_requests === 'write' &&
    (!candidate.changesWorkflow || permissions.workflows === 'write')
  );
}

module.exports = { canPublish };
