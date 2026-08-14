import { z } from 'zod'

// Zod mirror of contracts/schemas/discovery/instance-discovery.v1. The client
// validates the server's discovery document before trusting it, then evaluates
// its own version against the server's minimum (doc 16:65-66: the server returns
// the minimum supported version; the client drops to limited mode or forces an
// update). .passthrough() keeps additive server fields forward-compatible.

const secureHttpUrlSchema = z
  .string()
  .regex(
    /^(https:\/\/|http:\/\/127\.0\.0\.1(?::[0-9]+)?(?:\/|$)|http:\/\/\[::1\](?::[0-9]+)?(?:\/|$))/
  )
const secureWebSocketUrlSchema = z
  .string()
  .regex(/^(wss:\/\/|ws:\/\/127\.0\.0\.1(?::[0-9]+)?(?:\/|$)|ws:\/\/\[::1\](?::[0-9]+)?(?:\/|$))/)
const protocolVersionSchema = z.string().regex(/^[0-9]+\.[0-9]+$/)
const semanticVersionSchema = z.string().regex(/^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$/)
const instanceIdSchema = z
  .string()
  .min(3)
  .max(128)
  .regex(/^[A-Za-z0-9][A-Za-z0-9._-]+$/)

export const PieInstanceDiscoverySchema = z
  .object({
    schemaVersion: z.literal(1),
    instanceId: instanceIdSchema,
    displayName: z.string().min(1).max(120),
    deploymentType: z.enum(['saas', 'local_docker', 'self_hosted', 'on_prem']),
    apiBaseUrl: secureHttpUrlSchema,
    auth: z
      .object({
        protocol: z.literal('oidc'),
        issuer: secureHttpUrlSchema,
        clientId: z.string().min(1).max(128),
        redirectModes: z
          .array(z.enum(['loopback', 'private_uri_scheme']))
          .min(1)
          .refine((modes) => new Set(modes).size === modes.length)
      })
      .passthrough(),
    realtimeUrl: secureWebSocketUrlSchema,
    relayUrl: secureWebSocketUrlSchema.optional(),
    mediaUrl: secureHttpUrlSchema.optional(),
    protocol: z
      .object({
        api: protocolVersionSchema,
        realtime: protocolVersionSchema,
        relay: protocolVersionSchema.optional()
      })
      .passthrough(),
    minimumClientVersion: semanticVersionSchema,
    capabilities: z.record(z.string(), z.boolean()),
    expiresAt: z.string().datetime({ offset: true })
  })
  .passthrough()

export type PieInstanceDiscovery = z.infer<typeof PieInstanceDiscoverySchema>

// ── Client-side compatibility evaluation ────────────────────────────────────

export type ClientCompatibilityState = 'supported' | 'limited' | 'needs-update'

export type ClientCompatibility = {
  state: ClientCompatibilityState
  reasons: string[]
}

export type ClientCompatibilityInput = {
  appVersion: string
  supportedApiProtocol: string
  supportedRealtimeProtocol: string
}

function semverParts(version: string): [number, number, number] {
  const core = version.split('-')[0]!
  const [major = 0, minor = 0, patch = 0] = core.split('.').map((part) => Number.parseInt(part, 10))
  return [major, minor, patch]
}

function compareSemver(a: string, b: string): number {
  const left = semverParts(a)
  const right = semverParts(b)
  for (let i = 0; i < 3; i++) {
    if (left[i]! !== right[i]!) {
      return left[i]! < right[i]! ? -1 : 1
    }
  }
  return 0
}

function protocolParts(version: string): [number, number] {
  const [major = 0, minor = 0] = version.split('.').map((part) => Number.parseInt(part, 10))
  return [major, minor]
}

/**
 * Classifies the client against the server's discovery document:
 *  - needs-update: below the minimum version, or the server's protocol MAJOR is
 *    ahead (the client cannot speak the protocol at all).
 *  - limited: version + protocol major are fine, but the server's protocol MINOR
 *    is ahead — the client connects but some newer features are unavailable.
 *  - supported: fully compatible.
 */
export function evaluateClientCompatibility(
  input: ClientCompatibilityInput,
  discovery: PieInstanceDiscovery
): ClientCompatibility {
  if (compareSemver(input.appVersion, discovery.minimumClientVersion) < 0) {
    return { state: 'needs-update', reasons: ['below-minimum-version'] }
  }

  const reasons: string[] = []
  let state: ClientCompatibilityState = 'supported'

  const checks: { server: string; client: string; label: string }[] = [
    { server: discovery.protocol.api, client: input.supportedApiProtocol, label: 'api' },
    {
      server: discovery.protocol.realtime,
      client: input.supportedRealtimeProtocol,
      label: 'realtime'
    }
  ]
  for (const check of checks) {
    const [serverMajor, serverMinor] = protocolParts(check.server)
    const [clientMajor, clientMinor] = protocolParts(check.client)
    if (serverMajor > clientMajor) {
      return { state: 'needs-update', reasons: [`${check.label}-protocol-major-ahead`] }
    }
    if (serverMajor === clientMajor && serverMinor > clientMinor) {
      reasons.push(`${check.label}-protocol-minor-ahead`)
      state = 'limited'
    }
  }

  return { state, reasons }
}
