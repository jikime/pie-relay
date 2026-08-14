import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchRelayIdentity } from '../src/relay-gateway.ts'

describe('fetchRelayIdentity', () => {
  afterEach(() => vi.unstubAllGlobals())

  it('uses the identity verified by Relay', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          userId: 'member-a',
          profileId: 'pie-mobile',
          organizationId: 'workspace-a'
        }),
        { status: 200 }
      )
    )
    vi.stubGlobal('fetch', fetchMock)

    await expect(
      fetchRelayIdentity('https://relay.example', 'not-a-decodable-jwt', 'host-key-a')
    ).resolves.toEqual({
      userId: 'member-a',
      profileId: 'pie-mobile',
      organizationId: 'workspace-a'
    })
    expect(fetchMock).toHaveBeenCalledWith('https://relay.example/v1/identity', {
      method: 'GET',
      headers: { Authorization: 'Bearer not-a-decodable-jwt' }
    })
  })

  it('keeps legacy Relay compatibility without parsing token payloads', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('', { status: 404 })))
    await expect(
      fetchRelayIdentity('https://relay.example', 'opaque-token', 'host-key-a')
    ).resolves.toEqual({
      userId: 'host-key-a',
      profileId: 'pie-relay',
      organizationId: 'standalone'
    })
  })
})
