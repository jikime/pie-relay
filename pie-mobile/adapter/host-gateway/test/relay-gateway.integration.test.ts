import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { createHash, randomBytes } from 'node:crypto'
import { mkdtempSync, rmSync } from 'node:fs'
import { createServer } from 'node:net'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import NodeWebSocket from 'ws'
import { decodePairingOffer } from '../../../upstream/src/shared/pairing.ts'
import { connectMobileRelayForPairing } from '../../../upstream/mobile/src/transport/mobile-relay-physical-client.ts'
import { HostGateway } from '../src/host-gateway.ts'

const repoRoot = resolve('../../..')
const serverRoot = join(repoRoot, 'server')

async function reservePort(): Promise<number> {
  const server = createServer()
  await new Promise<void>((resolveListen, rejectListen) => {
    server.once('error', rejectListen)
    server.listen(0, '127.0.0.1', () => {
      server.off('error', rejectListen)
      resolveListen()
    })
  })
  const address = server.address()
  if (!address || typeof address === 'string') {
    throw new Error('Failed to reserve relay test port')
  }
  await new Promise<void>((resolveClose) => server.close(() => resolveClose()))
  return address.port
}

async function waitForHealthy(baseUrl: string, process: ChildProcess): Promise<void> {
  const deadline = Date.now() + 15_000
  while (Date.now() < deadline) {
    if (process.exitCode !== null) {
      throw new Error(`Pie Relay exited before becoming healthy (code ${process.exitCode})`)
    }
    try {
      const response = await fetch(`${baseUrl}/healthz`)
      if (response.ok) return
    } catch {
      // The listener is still starting.
    }
    await new Promise((resolveWait) => setTimeout(resolveWait, 50))
  }
  throw new Error('Timed out waiting for Pie Relay health check')
}

async function stopProcess(process: ChildProcess): Promise<void> {
  if (process.exitCode !== null || process.killed) return
  process.kill('SIGTERM')
  await Promise.race([
    new Promise<void>((resolveExit) => process.once('exit', () => resolveExit())),
    new Promise<void>((resolveTimeout) => setTimeout(resolveTimeout, 2_000))
  ])
  if (process.exitCode === null) process.kill('SIGKILL')
}

async function waitFor(
  predicate: () => boolean,
  detail: string,
  timeoutMs = 15_000
): Promise<void> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (predicate()) return
    await new Promise((resolveWait) => setTimeout(resolveWait, 50))
  }
  throw new Error(`Timed out waiting for ${detail}`)
}

function nodeSocket(url: string): WebSocket {
  return new NodeWebSocket(url, { perMessageDeflate: false }) as unknown as WebSocket
}

describe('Pie Relay mobile end-to-end', () => {
  let buildDir = ''
  let relayBinary = ''
  const gateways: HostGateway[] = []
  const processes: ChildProcess[] = []
  const dataDirs: string[] = []

  beforeAll(() => {
    buildDir = mkdtempSync(join(tmpdir(), 'pie-relay-server-build-'))
    relayBinary = join(buildDir, 'pie-relay-server')
    execFileSync('go', ['build', '-o', relayBinary, './cmd/relay'], {
      cwd: serverRoot,
      stdio: 'pipe'
    })
  }, 30_000)

  afterEach(async () => {
    await Promise.all(gateways.splice(0).map((gateway) => gateway.stop()))
    await Promise.all(processes.splice(0).map(stopProcess))
    for (const dataDir of dataDirs.splice(0)) {
      rmSync(dataDir, { recursive: true, force: true })
    }
  })

  afterAll(() => {
    if (buildDir) rmSync(buildDir, { recursive: true, force: true })
  })

  it('controls a real PTY and recovers its resume connection after the Go relay restarts', async () => {
    const port = await reservePort()
    const relayUrl = `http://127.0.0.1:${port}`
    const secret = `relay-test-${randomBytes(24).toString('hex')}`
    const token = execFileSync(
      relayBinary,
      ['--jwt-secret', secret, '--mint', 'pie-relay-test-host', '--mint-role', 'host'],
      { encoding: 'utf8' }
    ).trim()
    const relayStateDir = mkdtempSync(join(tmpdir(), 'pie-relay-state-'))
    dataDirs.push(relayStateDir)
    const relayArgs = [
      '--addr',
      `127.0.0.1:${port}`,
      '--jwt-secret',
      secret,
      '--mobile-public-url',
      relayUrl,
      '--mobile-state-file',
      join(relayStateDir, 'mobile.json')
    ]
    const relayProcess = spawn(relayBinary, relayArgs, { cwd: serverRoot, stdio: 'pipe' })
    processes.push(relayProcess)
    await waitForHealthy(relayUrl, relayProcess)

    const dataDir = mkdtempSync(join(tmpdir(), 'pie-relay-host-'))
    dataDirs.push(dataDir)
    const relayStatuses: string[] = []
    const gateway = new HostGateway({
      dataDir,
      cwd: repoRoot,
      advertiseHost: '127.0.0.1',
      listenHost: '127.0.0.1',
      port: 0,
      connectionMode: 'relay-only',
      relayUrl,
      relayToken: token,
      onRelayStatus: (status) => relayStatuses.push(status)
    })
    gateways.push(gateway)
    const ready = await gateway.start()
    const offer = decodePairingOffer(ready.pairingUrl)
    expect(offer.connectionMode).toBe('relay-only')
    expect(offer.relay).toBeDefined()
    expect(ready.relayEndpoint).toBe(relayUrl)

    const relay = offer.relay!
    const inviteClient = connectMobileRelayForPairing({
      relay,
      deviceToken: offer.deviceToken,
      desktopPublicKeyB64: offer.publicKeyB64,
      requestTimeoutMs: 10_000,
      createSocket: nodeSocket
    })
    const status = await inviteClient.sendRequest('status.get')
    expect(status).toMatchObject({ ok: true, result: { protocolVersion: 3, terminalCount: 1 } })

    const marker = `relay-e2e-${Date.now()}`
    await expect(
      inviteClient.sendRequest('terminal.send', {
        terminal: 'pie-relay-terminal-1',
        text: `printf '${marker}\\n'`,
        enter: true
      })
    ).resolves.toMatchObject({ ok: true })
    let terminalTail = ''
    for (let attempt = 0; attempt < 40 && !terminalTail.includes(marker); attempt++) {
      const read = await inviteClient.sendRequest('terminal.read', {
        terminal: 'pie-relay-terminal-1'
      })
      terminalTail = String(
        read.ok && typeof read.result === 'object' && read.result
          ? (read.result as { terminal?: { tail?: unknown } }).terminal?.tail ?? ''
          : ''
      )
      if (!terminalTail.includes(marker)) {
        await new Promise((resolveWait) => setTimeout(resolveWait, 25))
      }
    }
    expect(terminalTail).toContain(marker)

    const resumeToken = randomBytes(32).toString('base64url')
    const resumeTokenHash = createHash('sha256').update(resumeToken).digest('base64url')
    const installReqId = `install-${Date.now()}`
    const installed = await inviteClient.sendRequest('pairing.provisionRelay', {
      reqId: installReqId,
      newResumeTokenHash: resumeTokenHash
    })
    expect(installed).toMatchObject({
      ok: true,
      result: {
        v: 1,
        reqId: installReqId,
        authorizationMode: 'relay-basis',
        currentVersion: 1
      }
    })
    expect(installed.ok && installed.result).not.toHaveProperty('graceExpiresAt')
    const endpoints = await inviteClient.sendRequest('pairing.getEndpoints', { installReqId })
    expect(endpoints).toMatchObject({
      ok: true,
      result: {
        v: 1,
        installStatus: { state: 'committed', result: { reqId: installReqId } }
      }
    })
    inviteClient.close()

    // A Relay restart used to strand the mobile gateway in `offline` until
    // the desktop app itself was restarted. Keep the same gateway and durable
    // mobile credential alive, restart the real Go Relay, and require the
    // control channel to register itself again before the phone resumes.
    await stopProcess(relayProcess)
    const restartedRelay = spawn(relayBinary, relayArgs, { cwd: serverRoot, stdio: 'pipe' })
    processes.push(restartedRelay)
    await waitForHealthy(relayUrl, restartedRelay)
    await waitFor(
      () => relayStatuses.filter((status) => status === 'registered').length >= 2,
      'mobile Relay gateway re-registration'
    )

    const resumeClient = connectMobileRelayForPairing({
      relay,
      deviceToken: offer.deviceToken,
      desktopPublicKeyB64: offer.publicKeyB64,
      credential: resumeToken,
      expectedCredentialKind: 'resume',
      requestTimeoutMs: 10_000,
      createSocket: nodeSocket
    })
    await expect(resumeClient.sendRequest('status.get')).resolves.toMatchObject({
      ok: true,
      result: { graphStatus: 'ready', terminalCount: 1 }
    })
    resumeClient.close()
  }, 45_000)
})
