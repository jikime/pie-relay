import { z } from 'zod'

// Zod mirror of contracts/asyncapi/pie-realtime-v1.yaml + contracts/schemas/events/
// realtime-*.v1. Inbound (server->client) messages use .passthrough() so an
// additive optional field from a newer server is accepted (compat fixtures);
// outbound (client->server) messages we build ourselves are .strict().
export const PIE_REALTIME_PROTOCOL_VERSION = '1.0'

const opaqueIdSchema = z.string().uuid()
const instanceIdSchema = z
  .string()
  .min(3)
  .max(128)
  .regex(/^[A-Za-z0-9][A-Za-z0-9._-]+$/)
const cursorSchema = z.string().min(1).max(512)
const nullableCursorSchema = z.string().max(512).nullable()
const resourceVersionSchema = z.number().int().min(1)
const timestampSchema = z.string().datetime({ offset: true })

export const RESOURCE_CHANGE_RESOURCE_TYPES = [
  'organization',
  'membership',
  'team',
  'project',
  'work_item',
  'artifact',
  'agent_session',
  'operation',
  'permission',
  // Collaboration (chat). Additive to the union so the server's channel/message
  // invalidations pass client validation instead of being dropped. The backend
  // emits 'channel', 'message' (reactions/pins/edits/deletes all ride as a
  // 'message' change), and 'notification'; 'channel_member'/'read_cursor' are
  // declared server-side and added here so a future emit is not silently lost.
  'channel',
  'channel_member',
  'message',
  'read_cursor',
  'notification'
] as const
export const RESOURCE_CHANGE_KINDS = ['created', 'updated', 'archived', 'deleted'] as const

// ── Client -> server ────────────────────────────────────────────────────────

export const PieRealtimeClientHelloSchema = z
  .object({
    type: z.literal('client.hello'),
    schemaVersion: z.literal(1),
    protocolVersion: z.literal(PIE_REALTIME_PROTOCOL_VERSION),
    instanceId: instanceIdSchema,
    organizationId: opaqueIdSchema,
    lastCursor: z.string().max(512).nullable().optional(),
    capabilities: z.array(z.string().min(1).max(120)).optional()
  })
  .strict()

export const PieRealtimeHeartbeatSchema = z
  .object({
    type: z.literal('heartbeat'),
    schemaVersion: z.literal(1),
    direction: z.enum(['ping', 'pong']),
    sentAt: timestampSchema
  })
  .passthrough()

// ── Server -> client ────────────────────────────────────────────────────────

export const PieRealtimeServerWelcomeSchema = z
  .object({
    type: z.literal('server.welcome'),
    schemaVersion: z.literal(1),
    protocolVersion: z.literal(PIE_REALTIME_PROTOCOL_VERSION),
    connectionId: opaqueIdSchema,
    cursor: cursorSchema,
    heartbeatIntervalMs: z.number().int().min(1000).max(120000)
  })
  .passthrough()

export const PieRealtimeResourceChangedSchema = z
  .object({
    type: z.literal('resource.changed'),
    schemaVersion: z.literal(1),
    eventId: opaqueIdSchema,
    cursor: cursorSchema,
    organizationId: opaqueIdSchema,
    resourceType: z.enum(RESOURCE_CHANGE_RESOURCE_TYPES),
    resourceId: opaqueIdSchema,
    changeKind: z.enum(RESOURCE_CHANGE_KINDS),
    version: resourceVersionSchema,
    occurredAt: timestampSchema
  })
  .passthrough()

export const PieRealtimeSessionRevokedSchema = z
  .object({
    type: z.literal('session.revoked'),
    schemaVersion: z.literal(1),
    reason: z.enum([
      'user_logout',
      'admin_revoke',
      'account_disabled',
      'membership_revoked',
      'security_policy'
    ]),
    effectiveAt: timestampSchema
  })
  .passthrough()

export const PieRealtimeResyncRequiredSchema = z
  .object({
    type: z.literal('resync.required'),
    schemaVersion: z.literal(1),
    reason: z.enum(['cursor_expired', 'buffer_overflow', 'schema_mismatch', 'permission_changed']),
    cursor: nullableCursorSchema
  })
  .passthrough()

export const PieRealtimeConnectionClosingSchema = z
  .object({
    type: z.literal('connection.closing'),
    schemaVersion: z.literal(1),
    code: z.enum(['server_shutdown', 'session_revoked', 'protocol_unsupported', 'policy_changed']),
    reason: z.string().min(1).max(256),
    reconnect: z.boolean(),
    retryAfterMs: z.number().int().min(0).max(3600000).optional()
  })
  .passthrough()

// Ephemeral collaboration signals (presence/typing). Non-durable: no version, no
// cursor, no replay — the payload IS the state. The client renders directly and
// self-heals (typing on a short TTL, presence on the next presence event).
export const PieRealtimeTypingChangedSchema = z
  .object({
    type: z.literal('typing.changed'),
    schemaVersion: z.literal(1),
    organizationId: opaqueIdSchema,
    channelId: opaqueIdSchema,
    userId: opaqueIdSchema,
    at: timestampSchema
  })
  .passthrough()

export const PieRealtimePresenceChangedSchema = z
  .object({
    type: z.literal('presence.changed'),
    schemaVersion: z.literal(1),
    organizationId: opaqueIdSchema,
    userId: opaqueIdSchema,
    state: z.enum(['online', 'offline']),
    at: timestampSchema
  })
  .passthrough()

// Every inbound frame the client may receive, discriminated by `type`.
export const PieRealtimeServerMessageSchema = z.discriminatedUnion('type', [
  PieRealtimeServerWelcomeSchema,
  PieRealtimeResourceChangedSchema,
  PieRealtimeSessionRevokedSchema,
  PieRealtimeResyncRequiredSchema,
  PieRealtimeHeartbeatSchema,
  PieRealtimeConnectionClosingSchema,
  PieRealtimeTypingChangedSchema,
  PieRealtimePresenceChangedSchema
])

// Recovery feed page (contracts/schemas/resources/change-page.v1) fetched during
// resync; items are resource.changed messages.
export const PieResourceChangePageSchema = z
  .object({
    items: z.array(PieRealtimeResourceChangedSchema),
    nextCursor: cursorSchema.nullable(),
    hasMore: z.boolean()
  })
  .passthrough()

export type PieRealtimeClientHello = z.infer<typeof PieRealtimeClientHelloSchema>
export type PieRealtimeHeartbeat = z.infer<typeof PieRealtimeHeartbeatSchema>
export type PieRealtimeServerWelcome = z.infer<typeof PieRealtimeServerWelcomeSchema>
export type PieRealtimeResourceChanged = z.infer<typeof PieRealtimeResourceChangedSchema>
export type PieRealtimeSessionRevoked = z.infer<typeof PieRealtimeSessionRevokedSchema>
export type PieRealtimeResyncRequired = z.infer<typeof PieRealtimeResyncRequiredSchema>
export type PieRealtimeConnectionClosing = z.infer<typeof PieRealtimeConnectionClosingSchema>
export type PieRealtimeTypingChanged = z.infer<typeof PieRealtimeTypingChangedSchema>
export type PieRealtimePresenceChanged = z.infer<typeof PieRealtimePresenceChangedSchema>
// Non-durable collaboration frames (no cursor/version — the payload IS the state).
export type PieRealtimeEphemeral = PieRealtimeTypingChanged | PieRealtimePresenceChanged
export type PieRealtimeServerMessage = z.infer<typeof PieRealtimeServerMessageSchema>
export type PieResourceChangePage = z.infer<typeof PieResourceChangePageSchema>
