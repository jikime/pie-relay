import { mkdtempSync, rmSync } from 'node:fs'
import { createServer, type Server } from 'node:net'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import { WebSocket } from 'ws'
import { decodePairingOffer } from '../../../upstream/src/shared/pairing.ts'
import {
  decrypt,
  deriveSharedKey,
  encrypt,
  generateKeyPair,
  publicKeyFromBase64,
  publicKeyToBase64
} from '../../../upstream/src/shared/e2ee-crypto.ts'
import { HostGateway } from '../src/host-gateway.ts'

type MessageQueue = {
  next: () => Promise<string>
}

function queueMessages(ws: WebSocket): MessageQueue {
  const queued: string[] = []
  const waiters: Array<(value: string) => void> = []
  ws.on('message', (data) => {
    const value = data.toString()
    const waiter = waiters.shift()
    if (waiter) {
      waiter(value)
    } else {
      queued.push(value)
    }
  })
  return {
    next: () => {
      const value = queued.shift()
      return value === undefined
        ? new Promise<string>((resolveMessage) => waiters.push(resolveMessage))
        : Promise.resolve(value)
    }
  }
}

async function waitForOpen(ws: WebSocket): Promise<void> {
  if (ws.readyState === ws.OPEN) {
    return
  }
  await new Promise<void>((resolveOpen, reject) => {
    ws.once('open', resolveOpen)
    ws.once('error', reject)
  })
}

describe('HostGateway', () => {
  const dataDirs: string[] = []
  const gateways: HostGateway[] = []
  const servers: Server[] = []

  afterEach(async () => {
    await Promise.all(gateways.splice(0).map((gateway) => gateway.stop()))
    await Promise.all(
      servers.splice(0).map(
        (server) =>
          new Promise<void>((resolveClose) => server.close(() => resolveClose()))
      )
    )
    for (const dataDir of dataDirs.splice(0)) {
      rmSync(dataDir, { recursive: true, force: true })
    }
  })

  it('pairs with Pie Relay E2EE and streams a real PTY', { timeout: 15_000 }, async () => {
    const dataDir = mkdtempSync(join(tmpdir(), 'pie-relay-mobile-test-'))
    dataDirs.push(dataDir)
    const gateway = new HostGateway({
      dataDir,
      cwd: resolve('../../..'),
      advertiseHost: '127.0.0.1',
      listenHost: '127.0.0.1',
      port: 0,
      shell: '/bin/zsh'
    })
    gateways.push(gateway)
    const ready = await gateway.start()
    const offer = decodePairingOffer(ready.pairingUrl)

    const ws = new WebSocket(offer.endpoint)
    const messages = queueMessages(ws)
    await waitForOpen(ws)

    const clientKeys = generateKeyPair()
    const sharedKey = deriveSharedKey(
      clientKeys.secretKey,
      publicKeyFromBase64(offer.publicKeyB64)
    )
    ws.send(
      JSON.stringify({ type: 'e2ee_hello', publicKeyB64: publicKeyToBase64(clientKeys.publicKey) })
    )
    expect(JSON.parse(await messages.next())).toEqual({ type: 'e2ee_ready' })
    ws.send(encrypt(JSON.stringify({ type: 'e2ee_auth', deviceToken: offer.deviceToken }), sharedKey))
    expect(JSON.parse(decrypt(await messages.next(), sharedKey) ?? '')).toEqual({
      type: 'e2ee_authenticated'
    })

    ws.send(
      encrypt(
        JSON.stringify({
          id: 'status-1',
          deviceToken: offer.deviceToken,
          method: 'status.get'
        }),
        sharedKey
      )
    )
    const status = JSON.parse(decrypt(await messages.next(), sharedKey) ?? '')
    expect(status).toMatchObject({
      id: 'status-1',
      ok: true,
      result: { protocolVersion: 3, terminalCount: 1 }
    })

    for (const id of ['tabs-1', 'tabs-2']) {
      ws.send(
        encrypt(
          JSON.stringify({
            id,
            deviceToken: offer.deviceToken,
            method: 'session.tabs.list'
          }),
          sharedKey
        )
      )
    }
    const firstTabs = JSON.parse(decrypt(await messages.next(), sharedKey) ?? '')
    const secondTabs = JSON.parse(decrypt(await messages.next(), sharedKey) ?? '')
    expect(firstTabs.result.snapshotVersion).toBe(1)
    expect(secondTabs.result.snapshotVersion).toBe(firstTabs.result.snapshotVersion)

    ws.send(
      encrypt(
        JSON.stringify({
          id: 'stream-1',
          deviceToken: offer.deviceToken,
          method: 'terminal.subscribe',
          params: { terminal: 'pie-relay-terminal-1', viewport: { cols: 80, rows: 24 } }
        }),
        sharedKey
      )
    )
    const snapshot = JSON.parse(decrypt(await messages.next(), sharedKey) ?? '')
    expect(snapshot).toMatchObject({
      id: 'stream-1',
      ok: true,
      result: { type: 'scrollback', cols: 80, rows: 24 }
    })

    const marker = `gateway-${Date.now()}`
    ws.send(
      encrypt(
        JSON.stringify({
          id: 'send-1',
          deviceToken: offer.deviceToken,
          method: 'terminal.send',
          params: { terminal: 'pie-relay-terminal-1', text: `printf '${marker}\\n'`, enter: true }
        }),
        sharedKey
      )
    )

    let sawMarker = false
    let sawSendAck = false
    for (let index = 0; index < 20 && (!sawMarker || !sawSendAck); index++) {
      const message = JSON.parse(decrypt(await messages.next(), sharedKey) ?? '')
      if (message.id === 'send-1' && message.ok === true) {
        sawSendAck = true
      }
      if (message.id === 'stream-1' && String(message.result?.chunk ?? '').includes(marker)) {
        sawMarker = true
      }
    }
    expect(sawSendAck).toBe(true)
    expect(sawMarker).toBe(true)

    const [paired] = gateway.listDevices()
    expect(paired).toMatchObject({ deviceId: ready.deviceId, name: 'Pie Relay mobile' })
    expect(paired?.lastSeenAt).toBeGreaterThan(0)
    expect(gateway.revokeDevice(ready.deviceId)).toBe(true)
    expect(gateway.listDevices()).toEqual([])

    ws.close()
  })

  it('reuses fallback port so paired phones survive a restart', async () => {
    const preferredPortHolder = createServer()
    servers.push(preferredPortHolder)
    await new Promise<void>((resolveListen, rejectListen) => {
      preferredPortHolder.once('error', rejectListen)
      preferredPortHolder.listen(0, '127.0.0.1', () => {
        preferredPortHolder.off('error', rejectListen)
        resolveListen()
      })
    })
    const address = preferredPortHolder.address()
    if (!address || typeof address === 'string') {
      throw new Error('Failed to reserve preferred test port')
    }

    const dataDir = mkdtempSync(join(tmpdir(), 'pie-relay-mobile-fallback-test-'))
    dataDirs.push(dataDir)
    const options = {
      dataDir,
      cwd: resolve('../../..'),
      advertiseHost: '127.0.0.1',
      listenHost: '127.0.0.1',
      port: address.port,
      shell: '/bin/zsh'
    }

    const first = new HostGateway(options)
    gateways.push(first)
    const firstReady = await first.start()
    expect(firstReady.port).not.toBe(address.port)
    await first.stop()

    const second = new HostGateway(options)
    gateways.push(second)
    const secondReady = await second.start()
    expect(secondReady.port).toBe(firstReady.port)
  })
})
