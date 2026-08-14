import { AppState, Platform } from 'react-native'
import { connect, type RpcClient } from './rpc-client'
import { createStableLogicalRpcClient } from './stable-logical-rpc-client'
import type { ConnectionLogSink, HostProfile } from './types'
import { directPathForEndpoint } from './mobile-direct-endpoint-probe'
import { startMobileEndpointLifecycle } from './mobile-endpoint-lifecycle'

export function openHostLogicalClient(host: HostProfile, onLog: ConnectionLogSink): RpcClient {
  // Why: the stable facade owns app-visible RPC/subscription state while the
  // physical socket remains a replaceable generation. Relay-only profiles
  // must not briefly dial the legacy LAN endpoint while SecureStore loads the
  // resume credential, so they start from an inert disconnected generation.
  const relayOnly = host.connectionMode === 'relay-only'
  const logical = createStableLogicalRpcClient(
    relayOnly
      ? createRelayBootstrapClient()
      : connect(host.endpoint, host.deviceToken, host.publicKeyB64, { onLog }),
    relayOnly ? 'relay' : directPathForEndpoint(host, host.endpoint)
  )
  if (Platform.OS === 'web') {
    return logical
  }

  const endpointLifecycle = startMobileEndpointLifecycle(logical, host, onLog)
  endpointLifecycle.setForeground(AppState.currentState === 'active')
  const appStateSubscription = AppState.addEventListener('change', (state) => {
    endpointLifecycle.setForeground(state === 'active')
  })
  const closeLogical = logical.close
  logical.close = () => {
    appStateSubscription.remove()
    endpointLifecycle.stop()
    closeLogical()
  }
  const notifyLogicalForeground = logical.notifyForeground
  logical.notifyForeground = () => {
    endpointLifecycle.setForeground(true)
    notifyLogicalForeground()
  }
  return logical
}

function createRelayBootstrapClient(): RpcClient {
  return {
    sendRequest: () => Promise.reject(new Error('Relay session is starting')),
    subscribe: () => () => {},
    updateTerminalSubscriptionViewport: () => {},
    getState: () => 'disconnected',
    getReconnectAttempt: () => 0,
    getLastConnectedAt: () => null,
    onStateChange: () => () => {},
    notifyForeground: () => {},
    close: () => {}
  }
}
