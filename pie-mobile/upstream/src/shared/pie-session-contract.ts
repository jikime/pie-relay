import { z } from 'zod'

export const PIE_SESSION_GET_STATE_CHANNEL = 'pie:session:get-state'
export const PIE_SESSION_CHANGED_CHANNEL = 'pie:session:changed'
export const PIE_SESSION_PROTOCOL_VERSION = '1.0'
export const PIE_LOCAL_INSTANCE_ID = 'local-desktop'

const instanceIdSchema = z
  .string()
  .min(3)
  .max(128)
  .regex(/^[A-Za-z0-9][A-Za-z0-9._-]+$/)
const opaqueIdSchema = z.string().uuid()
const permissionSchema = z.string().regex(/^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_-]*)+$/)
const forbiddenSessionKeys = ['accessToken', 'refreshToken', 'idToken'] as const

export const PieSessionContextSchema = z.object({
  instanceId: instanceIdSchema,
  sessionId: opaqueIdSchema.nullable(),
  organizationId: opaqueIdSchema.nullable()
})

const signedOutSessionSchema = z
  .object({
    status: z.literal('signed_out'),
    instanceId: instanceIdSchema
  })
  .passthrough()

const authenticatedSessionSchema = z
  .object({
    status: z.enum(['signed_in', 'reauth_required']),
    instanceId: instanceIdSchema,
    userId: opaqueIdSchema,
    displayName: z.string().min(1).max(120),
    organizationId: opaqueIdSchema,
    permissions: z.array(permissionSchema).refine((items) => new Set(items).size === items.length),
    expiresAt: z.iso.datetime()
  })
  .passthrough()

export const PieSessionStateSchema = z
  .union([signedOutSessionSchema, authenticatedSessionSchema])
  .superRefine((session, context) => {
    for (const key of forbiddenSessionKeys) {
      if (key in session) {
        context.addIssue({
          code: 'custom',
          message: `Session state must not contain ${key}`,
          path: [key]
        })
      }
    }
  })

export const PieSessionGetRequestSchema = z
  .object({
    requestId: opaqueIdSchema,
    method: z.literal('session.getState'),
    protocolVersion: z.literal(PIE_SESSION_PROTOCOL_VERSION),
    sessionContext: PieSessionContextSchema,
    payload: z.object({}).strict()
  })
  .strict()

const problemDetailsSchema = z
  .object({
    type: z.string().min(1),
    title: z.string().min(1).max(200),
    status: z.number().int().min(400).max(599),
    detail: z.string().max(2000).optional(),
    instance: z.string().optional(),
    code: z.string().regex(/^[A-Z][A-Z0-9_]+$/),
    requestId: z.string().min(8).max(128)
  })
  .passthrough()

export const PieSessionGetResponseSchema = z.union([
  z
    .object({
      requestId: opaqueIdSchema,
      protocolVersion: z.literal(PIE_SESSION_PROTOCOL_VERSION),
      ok: z.literal(true),
      result: PieSessionStateSchema
    })
    .passthrough(),
  z
    .object({
      requestId: opaqueIdSchema,
      protocolVersion: z.literal(PIE_SESSION_PROTOCOL_VERSION),
      ok: z.literal(false),
      problem: problemDetailsSchema
    })
    .passthrough()
])

export const PieSessionChangedSchema = z
  .object({
    type: z.literal('session.changed'),
    protocolVersion: z.literal(PIE_SESSION_PROTOCOL_VERSION),
    sequence: z.number().int().min(1),
    session: PieSessionStateSchema
  })
  .passthrough()

export type PieSessionContext = z.infer<typeof PieSessionContextSchema>
export type PieSessionState = z.infer<typeof PieSessionStateSchema>
export type PieSessionGetRequest = z.infer<typeof PieSessionGetRequestSchema>
export type PieSessionGetResponse = z.infer<typeof PieSessionGetResponseSchema>
export type PieSessionChanged = z.infer<typeof PieSessionChangedSchema>

export type PieSessionRendererApi = {
  getState: () => Promise<PieSessionState>
  onChanged: (callback: (event: PieSessionChanged) => void) => () => void
}
