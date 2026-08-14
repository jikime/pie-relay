import { getAPIHandler } from '@/runtime.mjs'

export const runtime = 'nodejs'
export const dynamic = 'force-dynamic'
export const maxDuration = 300

function remoteAddress(request: Request) {
  return request.headers.get('x-real-ip')
    || request.headers.get('x-forwarded-for')?.split(',', 1)[0]?.trim()
    || 'unknown'
}

function handle(request: Request) {
  return getAPIHandler()(request, { remoteAddress: remoteAddress(request) })
}

export const GET = handle
export const POST = handle
export const PUT = handle
export const PATCH = handle
export const DELETE = handle
export const HEAD = handle
export const OPTIONS = handle
