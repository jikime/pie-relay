import assert from 'node:assert/strict'
import test from 'node:test'

import { createKrootCredential, isKrootPATCredential, ZERO_TIME } from '../src/kroot-credential.mjs'

test('creates the exact flat JSON shape consumed by krootauth.Credential', () => {
  const value = createKrootCredential({
    pat: ' kpat_test_secret ',
    serverURL: 'grpcs://api.example.test',
    relayURL: 'wss://relay.example.test/ws/agent',
    deviceID: '0123456789abcdef0123456789abcdef',
    updatedAt: '2026-07-28T01:02:03.000Z',
  })
  assert.deepEqual(value, {
    serverUrl: 'grpcs://api.example.test',
    accessToken: 'kpat_test_secret',
    expiresAt: ZERO_TIME,
    authKind: 'pat',
    updatedAt: '2026-07-28T01:02:03.000Z',
    relayUrl: 'wss://relay.example.test/ws/agent',
    deviceId: '0123456789abcdef0123456789abcdef',
  })
  assert.equal(isKrootPATCredential(value), true)
})

test('rejects missing PATs, embedded endpoint credentials, and invalid device identifiers', () => {
  assert.throws(() => createKrootCredential(), /pat is required/)
  assert.throws(() => createKrootCredential({ pat: 'x', serverURL: 'https://api.example.test' }), /serverURL/)
  assert.throws(() => createKrootCredential({ pat: 'x', relayURL: 'wss://user:secret@relay.example.test/ws' }), /relayURL/)
  assert.throws(() => createKrootCredential({ pat: 'x', deviceID: 'host-machine-name' }), /deviceID/)
})
