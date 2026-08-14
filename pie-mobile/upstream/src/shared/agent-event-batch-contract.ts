import { z } from 'zod'
import { SignedExecutionContextSchema } from './execution-context-contract'

// Wire contract for the R5 s1 org-level ingest endpoint
// `POST /v1/organizations/:organizationId/agent-events:batch`.
// This mirrors the server's AgentEventEnvelope (CloudEvents shape, doc 19 :203-236) and the
// batch request/response the ingest returns. The CLIENT stamps occurredAt (`time`) and
// `capturedAt`; the SERVER stamps receivedAt (never sent). Kept byte-for-byte with the
// persistence types in platform/packages/persistence/src/agent-event-ingest.ts.

export const AGENT_EVENT_PROTOCOL_VERSION = '1.0' as const

export const AgentEventProducerTypeSchema = z.enum([
  'hook',
  'transcript_reconciler',
  'runtime_observer',
  'mcp'
])

export const AgentEventTrustDomainSchema = z.enum([
  'client_observed',
  'provider_asserted',
  'server_verified'
])

export const AgentEventAssertionSchema = z.enum(['observed', 'declared', 'verified'])

const AgentEventContextSchema = z.object({
  projectId: z.string().nullable(),
  workItemId: z.string().nullable(),
  workspaceId: z.string().nullable(),
  hostId: z.string(),
  launchId: z.string().nullable(),
  agentSessionId: z.string(),
  agentRunId: z.string().nullable(),
  turnId: z.string().nullable()
})

const AgentEventProducerSchema = z.object({
  type: AgentEventProducerTypeSchema,
  provider: z.string(),
  parserVersion: z.string(),
  trustDomain: AgentEventTrustDomainSchema
})

// `payload`/`payloadObject` are opaque to the outbox — they are stored and forwarded verbatim,
// never inspected or logged. `.loose()` keeps forward-compatible fields the server may add.
export const AgentEventEnvelopeSchema = z
  .object({
    id: z.string().min(1),
    source: z.string(),
    type: z.string(),
    subject: z.string(),
    time: z.string(),
    pieorgid: z.string(),
    piestream: z.string().min(1),
    piesequence: z.number().int().nonnegative(),
    data: z
      .object({
        context: AgentEventContextSchema,
        producer: AgentEventProducerSchema,
        assertion: AgentEventAssertionSchema,
        classification: z.string(),
        visibility: z.string(),
        payload: z.record(z.string(), z.unknown()).optional(),
        payloadObject: z.record(z.string(), z.unknown()).optional(),
        correlationId: z.string().nullable().optional(),
        causationId: z.string().nullable().optional(),
        capturedAt: z.string()
      })
      .loose()
  })
  .loose()

export type AgentEventEnvelope = z.infer<typeof AgentEventEnvelopeSchema>

export const AgentEventBatchRequestSchema = z.object({
  batchId: z.string().min(1),
  producerId: z.string().min(1),
  protocolVersion: z.literal(AGENT_EVENT_PROTOCOL_VERSION),
  events: z.array(AgentEventEnvelopeSchema),
  clientCheckpoint: z.object({
    streamId: z.string().min(1),
    lastServerAck: z.number().int().nonnegative()
  }),
  // R5 s2b (ADDITIVE, optional): a signed, time-bounded, host-scoped ExecutionContext that binds
  // this batch's producer to its session cryptographically. Absent → the server falls back to the
  // s1 identity-based bind (local_observed), so existing identity-only ingest is unbroken.
  executionContext: SignedExecutionContextSchema.optional(),
  // R5 s5 anti-replay (ADDITIVE, optional): a one-time-use per-batch nonce. It rides the BATCH, not
  // the signed context, so the signed canonical form is unchanged. Keyed to batchId on the client
  // so an idempotent retry reuses it; the server rejects a consumed nonce re-presented with a
  // different batchId. Only meaningful (enforced) when a signed executionContext is present.
  submissionNonce: z.string().min(1).optional()
})

export type AgentEventBatchRequest = z.infer<typeof AgentEventBatchRequestSchema>

export const AgentEventItemStatusSchema = z.enum([
  'accepted',
  'duplicate',
  'retryable_rejected',
  'permanent_rejected'
])

export type AgentEventItemStatus = z.infer<typeof AgentEventItemStatusSchema>

export const AgentEventResultSchema = z.object({
  id: z.string(),
  status: AgentEventItemStatusSchema,
  code: z.string().optional(),
  retryAfterMs: z.number().optional()
})

// Per-stream gap report: `contiguousThrough` is the largest N with sequences 1..N all present;
// `gaps` are the missing sequences below the max seen. Used for gap-aware cursor progress, NOT
// for global cross-host ordering (doc 19: sequence is for gap detection only).
export const AgentStreamAckSchema = z.object({
  streamId: z.string(),
  contiguousThrough: z.number().int().nonnegative(),
  gaps: z.array(z.number().int().nonnegative())
})

export type AgentStreamAck = z.infer<typeof AgentStreamAckSchema>

export const AgentEventBatchResponseSchema = z.object({
  batchId: z.string(),
  results: z.array(AgentEventResultSchema),
  streamAcks: z.array(AgentStreamAckSchema)
})

export type AgentEventBatchResponse = z.infer<typeof AgentEventBatchResponseSchema>
