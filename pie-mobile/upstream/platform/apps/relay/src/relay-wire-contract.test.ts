import { randomBytes } from 'node:crypto'
import { expect, test } from 'vitest'
import { base64DecodedByteLength, parseRelayInbound } from './relay-wire-contract'

test('parses a valid join message', () => {
  const result = parseRelayInbound(
    JSON.stringify({
      type: 'join',
      protocolVersion: '1.0',
      sessionId: 's1',
      streamId: 'stream-1',
      credential: 'cap-token'
    })
  )
  expect(result.ok).toBe(true)
})

test('rejects malformed json', () => {
  expect(parseRelayInbound('{not json').ok).toBe(false)
})

test('rejects an unknown message type', () => {
  expect(parseRelayInbound(JSON.stringify({ type: 'attack' })).ok).toBe(false)
})

test('rejects a frame with an unknown direction', () => {
  const result = parseRelayInbound(
    JSON.stringify({
      type: 'frame',
      streamId: 'stream-1',
      seq: 1,
      dir: 'sideways',
      payload: 'AA=='
    })
  )
  expect(result.ok).toBe(false)
})

test('rejects a frame whose payload is not base64', () => {
  const result = parseRelayInbound(
    JSON.stringify({
      type: 'frame',
      streamId: 'stream-1',
      seq: 1,
      dir: 'output',
      payload: 'not*b64'
    })
  )
  expect(result.ok).toBe(false)
})

// The decoded-size helper must equal the true byte length WITHOUT decoding the
// content — this is what keeps maxFrameBytes enforcement opaque.
test('base64DecodedByteLength matches the real decoded length', () => {
  for (const size of [0, 1, 2, 3, 100, 4096, 65_537]) {
    const base64 = randomBytes(size).toString('base64')
    expect(base64DecodedByteLength(base64)).toBe(size)
  }
})
