import { afterEach, expect, test } from 'vitest'
// The relay itself NEVER imports these — this is an END-TO-END proof that two endpoint clients seal /
// open around an opaque relay (doc 34 §보안 제약 #5). The framing is the canonical mobile E2EE
// baseline at <repo>/src/shared (doc 32).
import {
  openMobileE2EEV2Frame,
  sealMobileE2EEV2Frame,
  type MobileE2EEDirection
} from '../../../../src/shared/mobile-e2ee-v2-framing'
import {
  joinRoom,
  nextMessage,
  send,
  startRelayHarness,
  type RelayHarness
} from './relay-integration-harness'

let harness: RelayHarness | undefined

afterEach(async () => {
  await harness?.close()
  harness = undefined
})

// A fixed shared key + session id stand in for the pairing handshake output; the point here is the
// relay's opacity, not the key exchange (covered by the mobile E2EE suite).
const SHARED_KEY = new Uint8Array(32).fill(7)
const SHARED_SESSION_ID = new Uint8Array(32).fill(9)
const DIRECTION: MobileE2EEDirection = 'desktop-to-mobile'
const COUNTER = 1n

test('E2EE payload round-trips through the relay while the relay only ever ferries ciphertext', async () => {
  harness = await startRelayHarness()
  const driver = await harness.connect()
  const viewer = await harness.connect()
  await joinRoom(driver, { sessionId: 's1', streamId: 'stream-1', credential: 'driver-cred' })
  await joinRoom(viewer, { sessionId: 's1', streamId: 'stream-1', credential: 'viewer-cred' })

  const plaintext = new TextEncoder().encode('rm -rf --dry-run /secret/path && echo done')
  const sealed = sealMobileE2EEV2Frame({
    payload: plaintext,
    key: SHARED_KEY,
    sessionId: SHARED_SESSION_ID,
    direction: DIRECTION,
    payloadKind: 'text',
    counter: COUNTER
  })
  // The relay forwards this base64 string verbatim; it is the SEALED bytes, never the plaintext.
  const wirePayload = Buffer.from(sealed).toString('base64')

  const received = nextMessage(viewer, (message) => message.type === 'frame')
  send(driver, { type: 'frame', streamId: 'stream-1', seq: 1, dir: 'output', payload: wirePayload })
  const frame = await received
  if (frame.type !== 'frame') throw new Error('expected a frame')

  // (b) The bytes the relay ferried are exactly the ciphertext — the relay never saw plaintext.
  const ferried = Buffer.from(frame.payload, 'base64')
  expect(ferried.equals(Buffer.from(sealed))).toBe(true)
  expect(ferried.includes(Buffer.from(plaintext))).toBe(false)

  // (a) Client B opens the ferried ciphertext back to the exact original plaintext.
  const opened = openMobileE2EEV2Frame({
    frame: ferried,
    key: SHARED_KEY,
    sessionId: SHARED_SESSION_ID,
    direction: DIRECTION,
    payloadKind: 'text',
    expectedCounter: COUNTER
  })
  expect(opened).not.toBeNull()
  expect(Buffer.from(opened!).equals(Buffer.from(plaintext))).toBe(true)
})

test('tampering the ferried ciphertext makes authenticated open fail (relay cannot alter payloads undetected)', async () => {
  harness = await startRelayHarness()
  const driver = await harness.connect()
  const viewer = await harness.connect()
  await joinRoom(driver, { sessionId: 's1', streamId: 'stream-1', credential: 'driver-cred' })
  await joinRoom(viewer, { sessionId: 's1', streamId: 'stream-1', credential: 'viewer-cred' })

  const plaintext = new TextEncoder().encode('confidential')
  const sealed = sealMobileE2EEV2Frame({
    payload: plaintext,
    key: SHARED_KEY,
    sessionId: SHARED_SESSION_ID,
    direction: DIRECTION,
    payloadKind: 'text',
    counter: COUNTER
  })

  const received = nextMessage(viewer, (message) => message.type === 'frame')
  send(driver, {
    type: 'frame',
    streamId: 'stream-1',
    seq: 1,
    dir: 'output',
    payload: Buffer.from(sealed).toString('base64')
  })
  const frame = await received
  if (frame.type !== 'frame') throw new Error('expected a frame')

  // Flip one ciphertext byte (past the 24-byte nonce prefix) to simulate any in-transit mutation.
  const ferried = Buffer.from(frame.payload, 'base64')
  ferried.writeUInt8(ferried.at(-1)! ^ 0xff, ferried.length - 1)
  const opened = openMobileE2EEV2Frame({
    frame: ferried,
    key: SHARED_KEY,
    sessionId: SHARED_SESSION_ID,
    direction: DIRECTION,
    payloadKind: 'text',
    expectedCounter: COUNTER
  })
  expect(opened).toBeNull()
})
