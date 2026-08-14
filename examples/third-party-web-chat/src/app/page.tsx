import { headers } from 'next/headers'
import { connection } from 'next/server'

import { WebChatApp } from '@/components/web-chat-app'
import { getPublicConfig } from '@/runtime.mjs'

export default async function Page() {
  await connection()
  const nonce = (await headers()).get('x-nonce') || ''
  const { registrationEnabled } = getPublicConfig()
  return <WebChatApp cspNonce={nonce} registrationEnabled={registrationEnabled} />
}
