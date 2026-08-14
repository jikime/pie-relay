// Zod-free wire surface for the generic Pie control-plane bridge. The SANDBOXED
// preload imports the channel + types from here (no runtime zod). Validation and
// the access token live in Main; the renderer only ever sends an org-relative
// path, so it can reach nothing outside its own organization's control-plane.

export const PIE_CONTROL_PLANE_CALL_CHANNEL = 'pie:control-plane:call'

export type PieControlPlaneMethod = 'GET' | 'POST' | 'PATCH' | 'DELETE'

export type PieControlPlaneRequest = {
  method: PieControlPlaneMethod
  // Org-relative path, e.g. '/change-requests' or '/knowledge/search?q=x'. Main
  // prefixes it with the authenticated org scope; it must start with '/'.
  path: string
  body?: unknown
  // OCC If-Match value (the resource etag) for a guarded update.
  ifMatch?: string
  // Idempotency-Key for a safe-to-retry create/action.
  idempotencyKey?: string
}

export type PieControlPlaneResponse = {
  ok: boolean
  status: number
  // Parsed JSON body, or null for a 204/empty response.
  data: unknown
}

export type PieControlPlaneRendererApi = {
  call: (request: PieControlPlaneRequest) => Promise<PieControlPlaneResponse>
}
