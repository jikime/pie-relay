import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { HostProfile } from './types'

const mocks = vi.hoisted(() => ({
  connect: vi.fn(),
  createStable: vi.fn(),
  startLifecycle: vi.fn()
}))

vi.mock('react-native', () => ({
  AppState: {
    currentState: 'active',
    addEventListener: vi.fn(() => ({ remove: vi.fn() }))
  },
  Platform: { OS: 'ios' }
}))
vi.mock('./rpc-client', () => ({ connect: mocks.connect }))
vi.mock('./stable-logical-rpc-client', () => ({
  createStableLogicalRpcClient: mocks.createStable
}))
vi.mock('./mobile-endpoint-lifecycle', () => ({
  startMobileEndpointLifecycle: mocks.startLifecycle
}))

import { openHostLogicalClient } from './host-logical-client'

const host: HostProfile = {
  id: 'host-1',
  name: 'Host 1',
  endpoint: 'ws://127.0.0.1:16893',
  deviceToken: 'device-token',
  publicKeyB64: 'public-key',
  lastConnected: 0
}

describe('host logical client bootstrap', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    const logical = {
      close: vi.fn(),
      notifyForeground: vi.fn()
    }
    mocks.createStable.mockReturnValue(logical)
    mocks.startLifecycle.mockReturnValue({
      setForeground: vi.fn(),
      stop: vi.fn()
    })
  })

  it('does not construct a LAN socket for relay-only hosts', () => {
    openHostLogicalClient({ ...host, connectionMode: 'relay-only' }, vi.fn())

    expect(mocks.connect).not.toHaveBeenCalled()
    expect(mocks.createStable).toHaveBeenCalledWith(
      expect.objectContaining({ getState: expect.any(Function) }),
      'relay'
    )
    const bootstrap = mocks.createStable.mock.calls[0]![0]
    expect(bootstrap.getState()).toBe('disconnected')
  })

  it('keeps automatic hosts on the authenticated direct-first path', () => {
    const direct = { id: 'direct-client' }
    mocks.connect.mockReturnValue(direct)

    openHostLogicalClient({ ...host, connectionMode: 'automatic' }, vi.fn())

    expect(mocks.connect).toHaveBeenCalledWith(
      host.endpoint,
      host.deviceToken,
      host.publicKeyB64,
      expect.any(Object)
    )
    expect(mocks.createStable).toHaveBeenCalledWith(direct, 'lan')
  })
})
