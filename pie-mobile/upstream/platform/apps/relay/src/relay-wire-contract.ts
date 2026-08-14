import { z } from 'zod'

// Zod wire contract for the relay's control/data messages (mirrors how
// src/shared/pie-realtime-contract.ts pins the realtime protocol). A full
// AsyncAPI document is a follow-up (TODO B1.1); this module is the source of
// truth meanwhile. Inbound (client->relay) messages are .strict() and validated
// on every message; outbound messages we build ourselves.
//
// Opaqueness invariant: on a data frame the relay reads ONLY the routing envelope
// (streamId, seq, dir) and forwards `payload` verbatim. `payload` is opaque
// base64 — the relay never decodes, parses, decrypts, or mutates it. Any
// client-supplied `sender` is ignored and overwritten with the authenticated id.

export const RELAY_PROTOCOL_VERSION = '1.0'

// Frame direction == traffic class. 'output' is bulk PTY/screen output (host or
// driver -> viewers). 'control' is control-input/audit (driver only) and is
// prioritized so a PTY flood cannot starve it (doc 07 / security constraint #6).
export const RELAY_FRAME_DIRECTIONS = ['output', 'control'] as const
export type RelayFrameDirection = (typeof RELAY_FRAME_DIRECTIONS)[number]

const streamIdSchema = z.string().min(1).max(128)
const sessionIdSchema = z.string().min(1).max(128)
// Base64 payload only — bounded so a single JSON message cannot balloon memory
// before the decoded-size check runs. The relay does not interpret the bytes.
const payloadSchema = z
  .string()
  .max(2 * 1024 * 1024)
  .regex(/^[A-Za-z0-9+/]*={0,2}$/, 'payload must be base64')

// ── Client -> relay (validated on every inbound message) ─────────────────────

export const RelayJoinSchema = z
  .object({
    type: z.literal('join'),
    protocolVersion: z.literal(RELAY_PROTOCOL_VERSION),
    sessionId: sessionIdSchema,
    streamId: streamIdSchema,
    // Opaque capability credential; secret, never logged. See AdmissionVerifier.
    credential: z.string().min(1).max(4096)
  })
  .strict()

export const RelayFrameInboundSchema = z
  .object({
    type: z.literal('frame'),
    streamId: streamIdSchema,
    seq: z.number().int().min(0),
    dir: z.enum(RELAY_FRAME_DIRECTIONS),
    payload: payloadSchema,
    // Accepted but IGNORED: the relay stamps the authenticated sender, so a
    // client cannot spoof identity by setting this. Present only for round-trip
    // ergonomics.
    sender: z.unknown().optional()
  })
  .strict()

export const RelayLeaveSchema = z
  .object({
    type: z.literal('leave')
  })
  .strict()

export const RelayInboundMessageSchema = z.discriminatedUnion('type', [
  RelayJoinSchema,
  RelayFrameInboundSchema,
  RelayLeaveSchema
])

export type RelayJoinMessage = z.infer<typeof RelayJoinSchema>
export type RelayFrameInboundMessage = z.infer<typeof RelayFrameInboundSchema>
export type RelayInboundMessage = z.infer<typeof RelayInboundMessageSchema>

// ── Relay -> client (built by the relay) ─────────────────────────────────────

export type RelayStampedSender = {
  participantId: string
  role: 'driver' | 'viewer'
}

export type RelayErrorCode =
  | 'malformed'
  | 'not_joined'
  | 'already_joined'
  | 'admission_denied'
  | 'forbidden_control'
  | 'frame_too_large'
  | 'rate_limited'

export type RelayOutboundMessage =
  | {
      type: 'join_ack'
      protocolVersion: typeof RELAY_PROTOCOL_VERSION
      sessionId: string
      streamId: string
      participantId: string
      role: 'driver' | 'viewer'
    }
  | {
      type: 'frame'
      streamId: string
      seq: number
      dir: RelayFrameDirection
      payload: string
      sender: RelayStampedSender
    }
  | { type: 'error'; code: RelayErrorCode; message: string }
  | { type: 'stream_lagged'; streamId: string; droppedFrames: number; dir: RelayFrameDirection }
  | { type: 'leave_ack' }

export function parseRelayInbound(
  raw: string
): { ok: true; message: RelayInboundMessage } | { ok: false } {
  let json: unknown
  try {
    json = JSON.parse(raw)
  } catch {
    return { ok: false }
  }
  const parsed = RelayInboundMessageSchema.safeParse(json)
  return parsed.success ? { ok: true, message: parsed.data } : { ok: false }
}

// Decoded byte length of a base64 string computed from its length alone — this
// deliberately does NOT decode the content, preserving payload opaqueness while
// still enforcing maxFrameBytes.
export function base64DecodedByteLength(base64: string): number {
  const length = base64.length
  if (length === 0) {
    return 0
  }
  let padding = 0
  if (base64.endsWith('==')) {
    padding = 2
  } else if (base64.endsWith('=')) {
    padding = 1
  }
  return Math.floor((length * 3) / 4) - padding
}
