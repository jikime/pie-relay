import { createServer, type Server as HttpServer } from 'node:http'
import { once } from 'node:events'
import type { AddressInfo } from 'node:net'
import { WebSocket } from 'ws'
import { createStubAdmissionVerifier, type AdmissionDecision } from './admission-verifier'
import { createRelayServer, type RelayServer } from './relay-server'
import { DEFAULT_RELAY_LIMITS, type RelayClock, type RelayLimits } from './relay-runtime-deps'
import type { RelayInboundMessage, RelayOutboundMessage } from './relay-wire-contract'

// Test-only harness: binds the relay to an ephemeral port and connects real `ws`
// clients so round-trip byte identity is proven end-to-end. NOT shipped — lives
// beside the tests and is only imported by *.test.ts.

export type RelayHarness = {
  url: string
  server: RelayServer
  httpServer: HttpServer
  connect: () => Promise<WebSocket>
  close: () => Promise<void>
}

export type HarnessOptions = {
  decide?: (sessionId: string, streamId: string, credential: string) => AdmissionDecision
  clock?: RelayClock
  limits?: RelayLimits
}

let participantCounter = 0

// Default admission: everyone is admitted; the FIRST distinct credential per
// call becomes a driver, others viewers — but tests usually pass an explicit
// decide() to control roles precisely.
function defaultDecide(_s: string, _st: string, credential: string): AdmissionDecision {
  participantCounter += 1
  const role = credential.startsWith('driver') ? 'driver' : 'viewer'
  return { ok: true, participantId: `p-${participantCounter}`, role }
}

export async function startRelayHarness(options: HarnessOptions = {}): Promise<RelayHarness> {
  const httpServer = createServer()
  const decide = options.decide ?? ((s, st, c) => defaultDecide(s, st, c))
  const server = createRelayServer({
    admission: createStubAdmissionVerifier((request) =>
      decide(request.sessionId, request.streamId, request.credential)
    ),
    clock: options.clock ?? { now: () => Date.now() },
    connectionIds: (() => {
      let n = 0
      return { next: () => `conn-${(n += 1)}` }
    })(),
    limits: options.limits ?? DEFAULT_RELAY_LIMITS,
    server: httpServer
  })
  httpServer.listen(0)
  await once(httpServer, 'listening')
  const port = (httpServer.address() as AddressInfo).port
  const url = `ws://127.0.0.1:${port}`

  const connect = async (): Promise<WebSocket> => {
    const ws = new WebSocket(url)
    await once(ws, 'open')
    return ws
  }

  const close = async (): Promise<void> => {
    // Terminate any still-open server-side sockets first; otherwise
    // httpServer.close() waits forever for lingering test connections.
    for (const client of server.wss.clients) {
      client.terminate()
    }
    await server.close()
    await new Promise<void>((resolve) => httpServer.close(() => resolve()))
  }

  return { url, server, httpServer, connect, close }
}

export function send(ws: WebSocket, message: RelayInboundMessage): void {
  ws.send(JSON.stringify(message))
}

// Resolve on the next message whose `type` matches; rejects on timeout so a
// missing/starved message fails the test instead of hanging.
export function nextMessage(
  ws: WebSocket,
  predicate: (message: RelayOutboundMessage) => boolean,
  timeoutMs = 2000
): Promise<RelayOutboundMessage> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      ws.off('message', onMessage)
      reject(new Error('timed out waiting for relay message'))
    }, timeoutMs)
    const onMessage = (data: unknown): void => {
      const message = JSON.parse(String(data)) as RelayOutboundMessage
      if (predicate(message)) {
        clearTimeout(timer)
        ws.off('message', onMessage)
        resolve(message)
      }
    }
    ws.on('message', onMessage)
  })
}

// Join a room and await the join_ack (or an error).
export async function joinRoom(
  ws: WebSocket,
  args: { sessionId: string; streamId: string; credential: string }
): Promise<RelayOutboundMessage> {
  const ack = nextMessage(ws, (message) => message.type === 'join_ack' || message.type === 'error')
  send(ws, {
    type: 'join',
    protocolVersion: '1.0',
    sessionId: args.sessionId,
    streamId: args.streamId,
    credential: args.credential
  })
  return ack
}
