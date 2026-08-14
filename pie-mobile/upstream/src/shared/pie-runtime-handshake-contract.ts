import { z } from 'zod'
import { PieSessionContextSchema } from './pie-session-contract'

export const PIE_RUNTIME_GET_HANDSHAKE_CHANNEL = 'pie:runtime:get-handshake'
export const PIE_RUNTIME_PROTOCOL_VERSION = '1.0'

const opaqueIdSchema = z.string().uuid()
const semanticVersionSchema = z.string().regex(/^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$/)
const runtimeCapabilitySchema = z.string().regex(/^[a-z][a-z0-9_.-]*$/)

const executionHostSchema = z
  .object({
    hostId: opaqueIdSchema,
    type: z.enum(['native', 'wsl', 'ssh', 'relay']),
    platform: z.enum(['darwin', 'linux', 'win32']),
    pathStyle: z.enum(['posix', 'windows']),
    caseSensitivePaths: z.boolean(),
    connectionId: z.string().max(256).nullable().optional(),
    wslDistribution: z.string().min(1).max(128).nullable().optional()
  })
  .passthrough()
  .superRefine((host, context) => {
    if (host.type === 'wsl' && !host.wslDistribution) {
      context.addIssue({
        code: 'custom',
        message: 'WSL hosts require a distribution',
        path: ['wslDistribution']
      })
    }
    if (host.pathStyle === 'windows' && host.platform !== 'win32') {
      context.addIssue({
        code: 'custom',
        message: 'Windows path style requires win32',
        path: ['platform']
      })
    }
  })

export const PieRuntimeHandshakeRequestSchema = z
  .object({
    type: z.literal('runtime.handshake'),
    requestId: opaqueIdSchema,
    mainVersion: semanticVersionSchema,
    supportedProtocolVersions: z
      .array(z.string().regex(/^[0-9]+\.[0-9]+$/))
      .min(1)
      .max(8)
      .refine((versions) => new Set(versions).size === versions.length),
    sessionContext: PieSessionContextSchema,
    capability: z.string().min(32).max(8192)
  })
  .strict()

export const PieRuntimeHandshakeResponseSchema = z
  .object({
    type: z.literal('runtime.welcome'),
    requestId: opaqueIdSchema,
    protocolVersion: z.literal(PIE_RUNTIME_PROTOCOL_VERSION),
    runtimeId: opaqueIdSchema,
    runtimeVersion: semanticVersionSchema,
    host: executionHostSchema,
    sqliteVersion: z.string().regex(/^[0-9]+\.[0-9]+\.[0-9]+$/),
    git: z
      .object({
        baseline: z.literal('2.25'),
        version: z
          .string()
          .regex(/^[0-9]+\.[0-9]+(?:\.[0-9]+)?$/)
          .nullable(),
        capabilities: z
          .array(runtimeCapabilitySchema)
          .refine((items) => new Set(items).size === items.length)
      })
      .passthrough(),
    providerParsers: z.record(z.string(), z.string().regex(/^[0-9]+\.[0-9]+\.[0-9]+$/)),
    capabilities: z
      .array(runtimeCapabilitySchema)
      .refine((items) => new Set(items).size === items.length),
    limits: z
      .object({
        maxRequestBytes: z.number().int().min(1024),
        maxFrameBytes: z.number().int().min(1024),
        maxConcurrentStreams: z.number().int().min(1).max(1024)
      })
      .passthrough()
  })
  .passthrough()

export type PieRuntimeHandshakeRequest = z.infer<typeof PieRuntimeHandshakeRequestSchema>
export type PieRuntimeHandshakeResponse = z.infer<typeof PieRuntimeHandshakeResponseSchema>

export type PieRuntimeRendererApi = {
  getHandshake: () => Promise<PieRuntimeHandshakeResponse>
}
