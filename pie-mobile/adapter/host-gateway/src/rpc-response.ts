import type { RpcResponse } from './types.ts'

export function success(
  runtimeId: string,
  id: string,
  result: unknown,
  streaming = false
): RpcResponse {
  return {
    id,
    ok: true,
    result,
    ...(streaming ? { streaming: true as const } : {}),
    _meta: { runtimeId }
  }
}

export function failure(
  runtimeId: string,
  id: string,
  code: string,
  message: string
): RpcResponse {
  return { id, ok: false, error: { code, message }, _meta: { runtimeId } }
}
