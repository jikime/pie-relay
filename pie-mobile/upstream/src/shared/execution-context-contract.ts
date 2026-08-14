import { z } from 'zod'

// R5 slice 2b: the SIGNED ExecutionContext + SessionBinding (doc 14 §R5 :811, doc 19 :216-231,
// doc 24 REM/CAP anti-forgery). This raises the producer↔session bind from an identity-only
// bearer match to a cryptographically signed, time-bounded, host-scoped binding: the batch's
// producer is bound to its session by an Ed25519 signature over a host-scoped context with a
// validity window, so a stale/forged/replayed context is rejected and a native·WSL·SSH launch at
// the SAME filesystem path can never be misattributed to another host's session.
//
// This module is the SINGLE source of the canonical bytes that get signed and verified. The
// platform workspace cannot import root `src/shared`, so it carries a byte-identical mirror
// (platform/packages/persistence/src/execution-context-canonical.ts) guarded by a shared golden
// fixture that BOTH sides verify — mirroring the relay-wire type-mirror precedent. If the two
// canonical serializers ever drift, the golden-fixture conformance test fails on both sides.

export const EXECUTION_CONTEXT_SCHEMA_VERSION = 1 as const

// Host discrimination is the point of this slice: the same workspacePath on a different host TYPE
// or host ID is a DIFFERENT binding, never merged (doc 14 exit condition :834).
export const ExecutionContextHostTypeSchema = z.enum(['native', 'wsl', 'ssh'])
export type ExecutionContextHostType = z.infer<typeof ExecutionContextHostTypeSchema>

export const ExecutionContextSchema = z
  .object({
    schemaVersion: z.literal(EXECUTION_CONTEXT_SCHEMA_VERSION),
    installationId: z.string().min(1),
    hostType: ExecutionContextHostTypeSchema,
    // A stable per-host identifier; the same path under a different hostId is a distinct binding.
    hostId: z.string().min(1),
    // Normalized per-host at the client seam BEFORE it enters the context (no `/` assumption).
    workspacePath: z.string().min(1),
    // The OS account the agent runs as. osUser-disambiguates-shared-host: two OS users on the SAME
    // SSH/build host at the SAME path are DISTINCT bindings (IDN-008). Remote for an SSH launch.
    osUser: z.string().min(1),
    launchId: z.string().min(1),
    agentSessionId: z.string().min(1),
    provider: z.string().min(1),
    // Validity window as epoch milliseconds (integers — no float/locale drift in canonical bytes).
    notBefore: z.number().int().nonnegative(),
    notAfter: z.number().int().nonnegative()
  })
  .strict()

export type ExecutionContext = z.infer<typeof ExecutionContextSchema>

export const SignedExecutionContextSchema = z
  .object({
    context: ExecutionContextSchema,
    // Repeated at the top level so a verifier can look up the registered key WITHOUT trusting the
    // inner context first; the verifier rejects a mismatch against context.installationId.
    installationId: z.string().min(1),
    // Ed25519 signature over the canonical bytes, base64.
    signature: z.string().min(1),
    // Fingerprint of the signing public key (rotation-detectable), base64url of sha256(SPKI DER).
    publicKeyId: z.string().min(1)
  })
  .strict()

export type SignedExecutionContext = z.infer<typeof SignedExecutionContextSchema>

// The EXACT bytes that get signed and verified. The field order and encoding are FROZEN — both
// workspaces must emit identical output or signatures will not verify across the boundary. Do not
// reorder, add, or reformat fields without bumping schemaVersion and updating the golden fixture.
export function canonicalizeExecutionContext(context: ExecutionContext): string {
  const parts = [
    `"schemaVersion":${JSON.stringify(context.schemaVersion)}`,
    `"installationId":${JSON.stringify(context.installationId)}`,
    `"hostType":${JSON.stringify(context.hostType)}`,
    `"hostId":${JSON.stringify(context.hostId)}`,
    `"workspacePath":${JSON.stringify(context.workspacePath)}`,
    `"osUser":${JSON.stringify(context.osUser)}`,
    `"launchId":${JSON.stringify(context.launchId)}`,
    `"agentSessionId":${JSON.stringify(context.agentSessionId)}`,
    `"provider":${JSON.stringify(context.provider)}`,
    `"notBefore":${JSON.stringify(context.notBefore)}`,
    `"notAfter":${JSON.stringify(context.notAfter)}`
  ]
  return `{${parts.join(',')}}`
}

// The signable byte buffer (UTF-8 of the canonical string). Ed25519 signs the message directly, so
// there is no separate digest step — the same bytes must be reproduced verbatim on the verify side.
export function executionContextSigningBytes(context: ExecutionContext): Buffer {
  return Buffer.from(canonicalizeExecutionContext(context), 'utf-8')
}

// A signed context is expired/not-yet-valid relative to an injected instant (server receivedAt or
// the client's pre-send check). Kept pure so both the pump's no-stale-context gate and the server's
// validity-window check use identical arithmetic.
export function isExecutionContextWithinWindow(context: ExecutionContext, atMs: number): boolean {
  return atMs >= context.notBefore && atMs <= context.notAfter
}
