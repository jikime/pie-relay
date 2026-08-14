import type { Server as HttpServer } from 'node:http'
import { WebSocketServer, type WebSocket } from 'ws'
import type { AdmissionVerifier } from './admission-verifier'
import { RelayConnection } from './relay-connection'
import { RelayRoomRegistry } from './relay-room'
import {
  DEFAULT_RELAY_LIMITS,
  NOOP_RELAY_LOGGER,
  type ConnectionIdSource,
  type RelayClock,
  type RelayLimits,
  type RelayLogger
} from './relay-runtime-deps'
import { createWsRelaySocket } from './relay-socket'

export type CreateRelayServerDeps = {
  admission: AdmissionVerifier
  clock: RelayClock
  connectionIds: ConnectionIdSource
  logger?: RelayLogger
  limits?: RelayLimits
  // Exactly one binding mode: attach to a provided http.Server (integration
  // tests bind it to an ephemeral port), listen on `port` directly, or run in
  // noServer mode where the caller drives `handleUpgrade`.
  server?: HttpServer
  port?: number
  noServer?: boolean
}

export type RelayServer = {
  wss: WebSocketServer
  registry: RelayRoomRegistry
  close: () => Promise<void>
}

export function createRelayServer(deps: CreateRelayServerDeps): RelayServer {
  const limits = deps.limits ?? DEFAULT_RELAY_LIMITS
  const logger = deps.logger ?? NOOP_RELAY_LOGGER
  const registry = new RelayRoomRegistry(logger)

  // Cap the raw WS message so a truly enormous payload is rejected by the
  // transport, while oversize-but-bounded frames still get a graceful app-level
  // error. Base64 inflates ~1.33x plus the JSON envelope, so allow generous slack.
  const maxPayload = limits.maxFrameBytes * 4 + 8 * 1024

  const wss = new WebSocketServer(
    deps.server
      ? { server: deps.server, maxPayload }
      : deps.noServer
        ? { noServer: true, maxPayload }
        : { port: deps.port ?? 0, maxPayload }
  )

  wss.on('connection', (ws: WebSocket, request) => {
    const socket = createWsRelaySocket(ws)
    // The connection self-manages via socket event handlers (message/close); it
    // needs no retained reference here.
    void new RelayConnection({
      socket,
      registry,
      admission: deps.admission,
      clock: deps.clock,
      connectionIds: deps.connectionIds,
      limits,
      logger,
      ...(request.socket.remoteAddress ? { remoteAddress: request.socket.remoteAddress } : {})
    })
  })

  return {
    wss,
    registry,
    close: () =>
      new Promise<void>((resolve, reject) => {
        wss.close((error) => (error ? reject(error) : resolve()))
      })
  }
}
