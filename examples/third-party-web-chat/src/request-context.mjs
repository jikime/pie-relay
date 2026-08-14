import { AsyncLocalStorage } from 'node:async_hooks'
import { randomUUID } from 'node:crypto'

const requestContext = new AsyncLocalStorage()
const SAFE_REQUEST_ID = /^[a-zA-Z0-9._:-]{8,128}$/

export function runWithRequestContext(request, callback) {
  const supplied = request.headers.get('x-request-id')?.trim() ?? ''
  const requestId = SAFE_REQUEST_ID.test(supplied) ? supplied : randomUUID()
  return requestContext.run(Object.freeze({ requestId }), callback)
}

export function currentRequestID() {
  return requestContext.getStore()?.requestId ?? ''
}
