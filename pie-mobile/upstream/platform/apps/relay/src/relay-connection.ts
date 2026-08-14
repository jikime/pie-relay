import type { AdmissionVerifier } from './admission-verifier'
import { FrameRateLimiter } from './frame-rate-limiter'
import { RelayRoom, RelayRoomRegistry, RoomMember } from './relay-room'
import type { ConnectionIdSource, RelayClock, RelayLimits, RelayLogger } from './relay-runtime-deps'
import type { RelaySocket } from './relay-socket'
import {
  base64DecodedByteLength,
  parseRelayInbound,
  RELAY_PROTOCOL_VERSION,
  type RelayErrorCode,
  type RelayInboundMessage,
  type RelayOutboundMessage
} from './relay-wire-contract'

// Application close codes (4000-4999 is the WS private-use range).
const CLOSE_ADMISSION_DENIED = 4403
const CLOSE_NORMAL = 1000

export type RelayConnectionDeps = {
  socket: RelaySocket
  registry: RelayRoomRegistry
  admission: AdmissionVerifier
  clock: RelayClock
  connectionIds: ConnectionIdSource
  limits: RelayLimits
  logger: RelayLogger
  remoteAddress?: string
}

// Owns ONE socket's lifecycle: join -> active -> leave/disconnect. The relay
// trusts the role assigned by admission and enforces frame size, rate, and
// single-driver control ownership; it never inspects frame payloads.
export class RelayConnection {
  private readonly connectionId: string
  private readonly rateLimiter: FrameRateLimiter
  private member: RoomMember | null = null
  private room: RelayRoom | null = null
  private sessionId = ''
  private streamId = ''
  private left = false

  constructor(private readonly deps: RelayConnectionDeps) {
    this.connectionId = deps.connectionIds.next()
    this.rateLimiter = new FrameRateLimiter(
      deps.clock,
      deps.limits.maxFramesPerSecond,
      deps.limits.maxBytesPerSecond
    )
    deps.socket.onMessage((data) => void this.onMessage(data))
    deps.socket.onClose(() => this.onClose())
  }

  private send(message: RelayOutboundMessage): void {
    this.deps.socket.send(JSON.stringify(message))
  }

  private sendError(code: RelayErrorCode, message: string): void {
    this.send({ type: 'error', code, message })
  }

  private async onMessage(data: string): Promise<void> {
    const parsed = parseRelayInbound(data)
    if (parsed.ok === false) {
      // Malformed/failed-schema control message: reject, keep the connection.
      this.sendError('malformed', 'message failed schema validation')
      return
    }
    const message = parsed.message
    if (this.member === null) {
      await this.handlePreJoin(message)
      return
    }
    this.handlePostJoin(message)
  }

  private async handlePreJoin(message: RelayInboundMessage): Promise<void> {
    if (message.type !== 'join') {
      this.sendError('not_joined', 'join is required before any other message')
      return
    }
    const decision = await this.deps.admission.verify({
      sessionId: message.sessionId,
      streamId: message.streamId,
      credential: message.credential,
      ...(this.deps.remoteAddress ? { remoteAddress: this.deps.remoteAddress } : {})
    })
    if (decision.ok === false) {
      // Never echo the credential or the raw reason detail into audit fields.
      this.deps.logger.warn(
        {
          event: 'relay.admission_denied',
          sessionId: message.sessionId,
          streamId: message.streamId
        },
        'admission denied'
      )
      this.sendError('admission_denied', 'admission denied')
      this.deps.socket.close(CLOSE_ADMISSION_DENIED, 'admission denied')
      return
    }
    this.sessionId = message.sessionId
    this.streamId = message.streamId
    this.member = new RoomMember(
      this.connectionId,
      {
        participantId: decision.participantId,
        role: decision.role,
        send: (serialized) => this.deps.socket.send(serialized),
        bufferedAmount: () => this.deps.socket.bufferedAmount(),
        limits: this.deps.limits
      },
      message.streamId
    )
    this.room = this.deps.registry.join(message.sessionId, message.streamId, this.member)
    this.deps.logger.info(
      {
        event: 'relay.joined',
        sessionId: message.sessionId,
        streamId: message.streamId,
        participantId: decision.participantId,
        role: decision.role
      },
      'connection joined room'
    )
    this.send({
      type: 'join_ack',
      protocolVersion: RELAY_PROTOCOL_VERSION,
      sessionId: message.sessionId,
      streamId: message.streamId,
      participantId: decision.participantId,
      role: decision.role
    })
  }

  private handlePostJoin(message: RelayInboundMessage): void {
    if (message.type === 'join') {
      this.sendError('already_joined', 'this connection already joined a room')
      return
    }
    if (message.type === 'leave') {
      this.send({ type: 'leave_ack' })
      this.leaveRoom()
      this.deps.socket.close(CLOSE_NORMAL, 'left')
      return
    }
    // frame
    const bytes = base64DecodedByteLength(message.payload)
    if (bytes > this.deps.limits.maxFrameBytes) {
      // Oversize frame is dropped with an error; the connection/room survives.
      this.sendError('frame_too_large', `frame exceeds ${this.deps.limits.maxFrameBytes} bytes`)
      return
    }
    if (this.rateLimiter.allow(bytes) === false) {
      this.sendError('rate_limited', 'frame rate/byte budget exceeded')
      return
    }
    const outcome = this.room?.route(this.member!, message)
    if (outcome && outcome.delivered === false) {
      this.sendError('forbidden_control', 'only the driver may send control frames')
    }
  }

  private leaveRoom(): void {
    if (this.left || this.member === null) {
      return
    }
    this.left = true
    // A disconnected driver's control ownership is NOT reassigned here — driver
    // arbitration lives in the Control Plane. The relay only reports the member
    // left; the room is GC'd if it was the last one.
    this.deps.registry.leave(this.sessionId, this.streamId, this.connectionId)
    this.deps.logger.info(
      {
        event: 'relay.left',
        sessionId: this.sessionId,
        streamId: this.streamId,
        connectionId: this.connectionId
      },
      'connection left room'
    )
  }

  private onClose(): void {
    this.leaveRoom()
  }
}
