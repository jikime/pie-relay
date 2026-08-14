import type { RelayRole } from './relay-runtime-deps'

// The relay admits a connection to a room only after an AdmissionVerifier vouches
// for it and assigns a role. This is the ONLY place identity/role is decided;
// the relay then trusts that role for the connection's lifetime (single-driver
// ownership is enforced against it). Kept injectable for B1: a live control-plane
// capability redemption is a B2 concern (see createControlPlaneAdmissionVerifier).

export type AdmissionRequest = {
  sessionId: string
  streamId: string
  // Opaque capability credential presented by the client. In production this is a
  // scoped, short-lived capability token redeemed at the control plane. It is a
  // secret: it MUST NOT be logged or echoed. The relay never interprets it here.
  credential: string
  remoteAddress?: string
}

export type AdmissionDecision =
  | { ok: true; participantId: string; role: RelayRole }
  | { ok: false; reason: string }

export type AdmissionVerifier = {
  verify: (request: AdmissionRequest) => Promise<AdmissionDecision>
}

// Test/dev double: the caller decides the outcome per request. Used by the B1
// suites so admission never depends on a running control plane.
export function createStubAdmissionVerifier(
  decide: (request: AdmissionRequest) => AdmissionDecision | Promise<AdmissionDecision>
): AdmissionVerifier {
  return { verify: async (request) => decide(request) }
}

// The capability kinds the control plane can grant. view = read-only; the three control kinds all
// mean "can drive". Kept as a local literal so the relay never imports control-plane types.
type CapabilityKind = 'view' | 'terminal_control' | 'desktop_control' | 'file_transfer'

// doc 34 B2: view→viewer; any control kind (terminal/desktop/file)→driver. The relay's single-driver
// ownership is then enforced against this role.
export function mapCapabilityToRole(capability: CapabilityKind): RelayRole {
  return capability === 'view' ? 'viewer' : 'driver'
}

// The credential a client presents to the relay (carried as the opaque `credential` string, encoded
// as JSON). organizationId scopes the redeem to the correct tenant; nonce is the single-use secret.
// The relay never interprets the nonce beyond passing it through — it is NEVER logged.
export type RelayAdmissionCredential = {
  organizationId: string
  nonce: string
}

// Structural subset of the global fetch so the verifier needs no DOM lib and tests can inject a stub.
type AdmissionResponse = { ok: boolean; status: number; json: () => Promise<unknown> }
export type AdmissionFetch = (
  url: string,
  init: { method: string; headers: Record<string, string>; body: string }
) => Promise<AdmissionResponse>

function parseCredential(credential: string): RelayAdmissionCredential | null {
  try {
    const parsed = JSON.parse(credential) as Partial<RelayAdmissionCredential>
    if (typeof parsed?.organizationId === 'string' && typeof parsed?.nonce === 'string') {
      return { organizationId: parsed.organizationId, nonce: parsed.nonce }
    }
  } catch {
    // fall through to fail-closed
  }
  return null
}

const CAPABILITY_KINDS: ReadonlySet<string> = new Set([
  'view',
  'terminal_control',
  'desktop_control',
  'file_transfer'
])

// B2: redeem the presented capability at the control plane's operator-gated relay-admit endpoint and
// map the granted kind to a relay role. Fail-closed: a missing/parse-failed credential, any network
// or parse error, or a non-200 response yields {ok:false} — the relay never admits unverified.
export function createControlPlaneAdmissionVerifier(config: {
  controlPlaneBaseUrl: string
  operatorToken: string
  fetchImpl?: AdmissionFetch
}): AdmissionVerifier {
  const doFetch = config.fetchImpl ?? (globalThis.fetch as unknown as AdmissionFetch)
  const base = config.controlPlaneBaseUrl.replace(/\/+$/, '')
  return {
    verify: async (request) => {
      const credential = parseCredential(request.credential)
      if (!credential) {
        return { ok: false, reason: 'invalid_credential' }
      }
      try {
        const response = await doFetch(
          `${base}/internal/remote-sessions/${encodeURIComponent(request.sessionId)}/relay-admit`,
          {
            method: 'POST',
            headers: {
              authorization: `Bearer ${config.operatorToken}`,
              'content-type': 'application/json'
            },
            // audience is the stream the grant is bound to; the nonce is a secret and never logged.
            body: JSON.stringify({
              nonce: credential.nonce,
              audience: request.streamId,
              organizationId: credential.organizationId
            })
          }
        )
        if (!response.ok) {
          return { ok: false, reason: `admission_rejected_${response.status}` }
        }
        const grant = (await response.json()) as { participantId?: unknown; capability?: unknown }
        if (
          typeof grant?.participantId !== 'string' ||
          typeof grant?.capability !== 'string' ||
          !CAPABILITY_KINDS.has(grant.capability)
        ) {
          return { ok: false, reason: 'admission_malformed' }
        }
        return {
          ok: true,
          participantId: grant.participantId,
          role: mapCapabilityToRole(grant.capability as CapabilityKind)
        }
      } catch {
        // Network/parse failure → fail closed. The error is intentionally not logged (may reference
        // the request); the relay refuses the connection rather than admitting it.
        return { ok: false, reason: 'admission_unavailable' }
      }
    }
  }
}
