import { afterEach, expect, test } from 'vitest'
// Drives the Orca client PTY-relay bridge (repo <root>/src/main/pty-relay) end to
// end through the REAL relay, proving the host→relay→viewer data path over the
// merged B1/B2 relay. The relay itself NEVER imports the E2EE seal/open — only
// the client endpoints do (doc 34 §보안 제약 #5). Mirrors relay-e2ee-e2e.test.ts.
import {
  createPtyFrameOpener,
  createPtyFrameSealer
} from '../../../../src/main/pty-relay/pty-relay-e2ee'
import { createPtyRelayHost } from '../../../../src/main/pty-relay/pty-relay-host'
import {
  createPtyRelayViewer,
  type PtyRelayViewer
} from '../../../../src/main/pty-relay/pty-relay-viewer'
import { connectWsRelayClientSocket } from '../../../../src/main/pty-relay/relay-client-socket'
import type { RelayConnect } from '../../../../src/main/pty-relay/relay-client-socket'
import type { PtyOutputSource } from '../../../../src/main/pty-relay/pty-output-source'
import { startRelayHarness, type RelayHarness } from './relay-integration-harness'

let harness: RelayHarness | undefined

afterEach(async () => {
  await harness?.close()
  harness = undefined
})

// Fixed shared endpoint key stands in for the pairing handshake output; the point
// here is the relay's opacity, not the key exchange.
const SHARED = { key: new Uint8Array(32).fill(7), e2eeSessionId: new Uint8Array(32).fill(9) }
const seal = createPtyFrameSealer(SHARED)
const open = createPtyFrameOpener(SHARED)

const enc = (text: string): Uint8Array => new TextEncoder().encode(text)
const dec = (bytes: Uint8Array): string => new TextDecoder().decode(bytes)

async function waitFor(predicate: () => boolean, timeoutMs = 2000): Promise<void> {
  const start = Date.now()
  while (!predicate()) {
    if (Date.now() - start > timeoutMs) {
      throw new Error('timed out waiting for condition')
    }
    await new Promise((resolve) => setTimeout(resolve, 5))
  }
}

function createFakeSource(snapshot: Uint8Array | null): PtyOutputSource & {
  emit(chunk: Uint8Array): void
  fireExit(): void
} {
  const dataCbs: ((chunk: Uint8Array) => void)[] = []
  const exitCbs: (() => void)[] = []
  return {
    onData(cb) {
      dataCbs.push(cb)
      return () => {}
    },
    onExit(cb) {
      exitCbs.push(cb)
      return () => {}
    },
    snapshot: () => snapshot,
    emit(chunk) {
      for (const cb of dataCbs) {
        cb(chunk)
      }
    },
    fireExit() {
      for (const cb of exitCbs) {
        cb()
      }
    }
  }
}

// Records outbound frames sent by the wrapped socket (view-only proof).
function recordingConnect(): { connect: RelayConnect; sent: string[] } {
  const sent: string[] = []
  const connect: RelayConnect = async (url) => {
    const sock = await connectWsRelayClientSocket(url)
    return {
      send: (data) => {
        sent.push(data)
        sock.send(data)
      },
      onMessage: (cb) => sock.onMessage(cb),
      onClose: (cb) => sock.onClose(cb),
      close: () => sock.close()
    }
  }
  return { connect, sent }
}

function makeViewer(connect: RelayConnect): PtyRelayViewer {
  return createPtyRelayViewer({
    relayUrl: harness!.url,
    sessionId: 's1',
    streamId: 'stream-1',
    credential: 'viewer-1',
    open,
    connect
  })
}

function makeHost(source: PtyOutputSource, connect: RelayConnect) {
  return createPtyRelayHost({
    outputSource: source,
    relayUrl: harness!.url,
    sessionId: 's1',
    streamId: 'stream-1',
    credential: 'viewer-host', // any role may send `output`; view-only
    seal,
    connect
  })
}

test('PTY output round-trips host→REAL relay→viewer in order, snapshot first', async () => {
  harness = await startRelayHarness()
  const source = createFakeSource(enc('SNAPSHOT-SEED'))
  const viewer = makeViewer(recordingConnect().connect)
  await viewer.start()
  const host = makeHost(source, recordingConnect().connect)
  await host.start()

  source.emit(enc('alpha\n'))
  source.emit(enc('beta\n'))

  await waitFor(() => viewer.received().length === 3)
  expect(dec(viewer.received()[0]!)).toBe('SNAPSHOT-SEED') // catch-up first
  expect(dec(viewer.received()[1]!)).toBe('alpha\n')
  expect(dec(viewer.received()[2]!)).toBe('beta\n')
})

test('exit propagates and the viewer only ever sends join/leave through the REAL relay', async () => {
  harness = await startRelayHarness()
  const source = createFakeSource(null)
  const viewerRec = recordingConnect()
  const viewer = makeViewer(viewerRec.connect)
  let exited = false
  viewer.onExit(() => {
    exited = true
  })
  await viewer.start()
  const host = makeHost(source, recordingConnect().connect)
  await host.start()

  source.emit(enc('output\n'))
  await waitFor(() => viewer.received().length === 1)
  source.fireExit()
  await waitFor(() => exited)

  await viewer.stop()
  const sentTypes = viewerRec.sent.map((raw) => (JSON.parse(raw) as { type: string }).type)
  expect(sentTypes).toEqual(['join', 'leave'])
})
