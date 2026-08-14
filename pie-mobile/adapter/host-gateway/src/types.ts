export type RpcRequest = {
  id: string
  deviceToken: string
  method: string
  params?: Record<string, unknown>
}

export type RpcResponse =
  | {
      id: string
      ok: true
      result: unknown
      streaming?: true
      _meta: { runtimeId: string }
    }
  | {
      id: string
      ok: false
      error: { code: string; message: string }
      _meta: { runtimeId: string }
    }

export type Reply = (response: string) => void
