import type { AuthenticatedMobileSocket, MobileSocketWiring } from '../../../upstream/src/main/runtime/rpc/mobile-socket-wiring.ts'
import type { E2EEKeypair } from '../../../upstream/src/main/runtime/e2ee-keypair.ts'
import { RelayControlClient } from '../../../upstream/src/main/runtime/relay/relay-control-client.ts'
import { CloudRelayTransport } from '../../../upstream/src/main/runtime/rpc/relay-transport.ts'
import {
  deriveRelayHostId,
  requestRelayAssignment
} from '../../../upstream/src/main/runtime/relay/relay-http-client.ts'
import type { PairingRelay } from '../../../upstream/src/shared/mobile-relay-pairing-offer.ts'
import type {
  PairingGetEndpointsParams,
  PairingGetEndpointsResult,
  PairingProvisionRelayParams,
  DeviceCredentialInstalled,
  MobileRelayEndpoint
} from '../../../upstream/src/shared/mobile-relay-credential-contract.ts'

export type RelayGatewayStatus = 'disabled' | 'connecting' | 'registered' | 'offline'

export type RelayGatewayOptions = {
  relayUrl: string
  relayToken: string
  keypair: E2EEKeypair
  mobileSocketWiring: MobileSocketWiring
  onStatus?: (status: RelayGatewayStatus, detail?: string) => void
}

const RECONNECT_BASE_DELAY_MS = 500
const RECONNECT_MAX_DELAY_MS = 15_000

function relayOrigin(value: string): string {
  const parsed = new URL(value.trim())
  if (parsed.protocol === 'ws:') parsed.protocol = 'http:'
  if (parsed.protocol === 'wss:') parsed.protocol = 'https:'
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    throw new Error('Relay URL must use http(s) or ws(s)')
  }
  parsed.pathname = '/'
  parsed.search = ''
  parsed.hash = ''
  return parsed.origin
}

type VerifiedRelayIdentity = { userId: string; profileId: string; organizationId: string }

export async function fetchRelayIdentity(
  directorUrl: string,
  relayToken: string,
  fallbackUserId: string
): Promise<VerifiedRelayIdentity> {
  const response = await fetch(`${directorUrl}/v1/identity`, {
    method: 'GET',
    headers: { Authorization: `Bearer ${relayToken}` }
  })
  // A legacy Relay does not expose the verified identity endpoint. Preserve
  // connectivity without decoding an unverified JWT payload in the gateway;
  // the E2EE host key remains the stable compatibility identity.
  if (response.status === 404) {
    return { userId: fallbackUserId, profileId: 'pie-relay', organizationId: 'standalone' }
  }
  if (!response.ok) {
    throw new Error(`Relay identity request failed: HTTP ${response.status}`)
  }
  const value = (await response.json()) as Partial<VerifiedRelayIdentity>
  if (!value.userId || !value.profileId || !value.organizationId) {
    throw new Error('Relay identity response is incomplete')
  }
  return {
    userId: value.userId,
    profileId: value.profileId,
    organizationId: value.organizationId
  }
}

export class RelayGateway {
  private readonly options: RelayGatewayOptions
  private readonly directorUrl: string
  private readonly relayHostId: string
  private control: RelayControlClient | null = null
  private transport: CloudRelayTransport | null = null
  private transportCellUrl: string | null = null
  private endpoint: MobileRelayEndpoint | null = null
  private stopped = true
  private connectionAttempt = 0
  private reconnectAttempt = 0
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private connectPromise: Promise<void> | null = null
  private previousGeneration: number | undefined
  private controlResumeSecret: string | undefined

  constructor(options: RelayGatewayOptions) {
    this.options = options
    this.directorUrl = relayOrigin(options.relayUrl)
    this.relayHostId = deriveRelayHostId(options.keypair.publicKey)
  }

  async start(): Promise<void> {
    if (!this.stopped) {
      if (this.control) return
      if (this.connectPromise) return this.connectPromise
    }
    this.stopped = false
    const promise = this.establishControl()
    this.connectPromise = promise
    try {
      await promise
    } catch (error) {
      this.stopped = true
      throw error
    } finally {
      if (this.connectPromise === promise) this.connectPromise = null
    }
  }

  private async establishControl(): Promise<void> {
    this.options.onStatus?.('connecting')
    const attempt = ++this.connectionAttempt
    const [assignment, identity] = await Promise.all([
      requestRelayAssignment({
        directorUrl: this.directorUrl,
        relayToken: this.options.relayToken,
        relayHostId: this.relayHostId
      }),
      fetchRelayIdentity(this.directorUrl, this.options.relayToken, this.relayHostId)
    ])

    const previousTransport = this.transport
    const reusingTransport =
      previousTransport !== null && this.transportCellUrl === assignment.cellUrl
    const transport =
      reusingTransport
        ? previousTransport
        : new CloudRelayTransport({
            cellUrl: assignment.cellUrl,
            relayHostId: this.relayHostId,
            generation: 0
          })
    if (!reusingTransport) {
      this.options.mobileSocketWiring.attachTransport(transport, (socket) =>
        transport.metadataFor(socket)
      )
    }

    let control!: RelayControlClient
    let activated = false
    control = new RelayControlClient({
      cellUrl: assignment.cellUrl,
      relayJwt: this.options.relayToken,
      relayHostId: this.relayHostId,
      assignmentEpoch: assignment.assignmentEpoch,
      identity,
      keypair: this.options.keypair,
      appVersion: 'Pie Relay 0.1.0',
      ...(this.previousGeneration === undefined
        ? {}
        : { previousGeneration: this.previousGeneration }),
      ...(this.controlResumeSecret ? { controlResumeSecret: this.controlResumeSecret } : {}),
      onConnectionOpen: (connection) => {
        void transport.openConnection(connection).catch((error) =>
          this.options.onStatus?.('offline', error instanceof Error ? error.message : String(error))
        )
      },
      onDrain: () => {
        if (this.control !== control || this.stopped) return
        this.options.onStatus?.('offline', 'Relay server requested migration')
        control.closeNow()
      },
      onClose: () => {
        if (!activated || this.control !== control || this.stopped) return
        this.control = null
        this.scheduleReconnect('Relay control disconnected')
      }
    })
    try {
      await transport.start()
      const ack = await control.connect()
      if (this.stopped || attempt !== this.connectionAttempt) {
        control.closeNow()
        if (!reusingTransport) {
          this.options.mobileSocketWiring.detachTransport(transport)
          await transport.stop()
        }
        return
      }
      transport.setGeneration(ack.generation)
      this.previousGeneration = ack.generation
      this.controlResumeSecret = ack.controlResumeSecret
    } catch (error) {
      control.closeNow()
      if (!reusingTransport) {
        this.options.mobileSocketWiring.detachTransport(transport)
        await transport.stop()
      }
      this.options.onStatus?.('offline', error instanceof Error ? error.message : String(error))
      throw error
    }

    activated = true
    this.control = control
    this.transport = transport
    this.transportCellUrl = assignment.cellUrl
    this.endpoint = {
      v: 1,
      directorUrl: this.directorUrl,
      cellUrl: assignment.cellUrl,
      assignmentEpoch: assignment.assignmentEpoch,
      relayHostId: this.relayHostId,
      e2eeFraming: 2
    }
    if (previousTransport && previousTransport !== transport) {
      this.options.mobileSocketWiring.detachTransport(previousTransport)
      await previousTransport.stop()
    }
    this.reconnectAttempt = 0
    this.options.onStatus?.('registered')
  }

  async stop(): Promise<void> {
    this.stopped = true
    this.connectionAttempt++
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    const control = this.control
    const transport = this.transport
    this.control = null
    this.transport = null
    this.transportCellUrl = null
    this.endpoint = null
    this.previousGeneration = undefined
    this.controlResumeSecret = undefined
    control?.closeNow()
    if (transport) {
      this.options.mobileSocketWiring.detachTransport(transport)
      await transport.stop()
    }
    this.options.onStatus?.('disabled')
  }

  private scheduleReconnect(detail: string): void {
    if (this.stopped || this.reconnectTimer || this.connectPromise || this.control) return
    this.options.onStatus?.('offline', detail)
    const delay = Math.min(
      RECONNECT_MAX_DELAY_MS,
      RECONNECT_BASE_DELAY_MS * 2 ** Math.min(this.reconnectAttempt, 5)
    )
    this.reconnectAttempt++
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      if (this.stopped || this.control) return
      const promise = this.establishControl()
      this.connectPromise = promise
      void promise
        .catch((error: unknown) => {
          this.options.onStatus?.(
            'offline',
            error instanceof Error ? error.message : String(error)
          )
        })
        .finally(() => {
          if (this.connectPromise === promise) this.connectPromise = null
          if (!this.stopped && !this.control) {
            this.scheduleReconnect('Relay reconnect pending')
          }
        })
    }, delay)
    ;(this.reconnectTimer as unknown as { unref?: () => void }).unref?.()
  }

  async createPairingRelay(relayDeviceId: string): Promise<PairingRelay> {
    const control = this.requireControl()
    const endpoint = this.requireEndpoint()
    const invite = await control.createInvite(relayDeviceId)
    return {
      ...endpoint,
      inviteToken: invite.inviteToken,
      inviteExpiresAt: invite.expiresAt
    }
  }

  async getEndpoints(
    socket: AuthenticatedMobileSocket,
    params: PairingGetEndpointsParams
  ): Promise<PairingGetEndpointsResult> {
    const control = this.requireControl()
    const result: PairingGetEndpointsResult = { v: 1, relay: this.requireEndpoint() }
    if (params.installReqId) {
      const { type: _type, ...status } = await control.credentialInstallStatus(
        socket.device.deviceId,
        params.installReqId
      )
      result.installStatus = status
    }
    if (params.resumeConfirmReqId) {
      if (socket.transport.transport !== 'relay' || socket.transport.credentialKind !== 'resume') {
        throw new Error('resume_confirmation_unavailable')
      }
      const { type: _type, ...confirmation } = await control.confirmResume(
        socket.transport.basisConnId,
        params.resumeConfirmReqId
      )
      result.resumeConfirmation = confirmation
    }
    return result
  }

  async provisionRelay(
    socket: AuthenticatedMobileSocket,
    params: PairingProvisionRelayParams
  ): Promise<DeviceCredentialInstalled> {
    const authorization =
      socket.transport.transport === 'direct'
        ? { mode: 'authenticated-direct' as const, directAuthId: socket.connectionId }
        : socket.transport.credentialKind === 'invite'
          ? { mode: 'relay-basis' as const, basisConnId: socket.transport.basisConnId }
          : null
    if (!authorization) throw new Error('relay_provision_authorization_unavailable')
    const { type: _type, ...installed } = await this.requireControl().installCredential({
      ...params,
      relayDeviceId: socket.device.deviceId,
      authorization
    })
    return installed
  }

  async revokeDevice(relayDeviceId: string): Promise<void> {
    if (!this.control) return
    await this.control.revokeDevice(relayDeviceId)
  }

  private requireControl(): RelayControlClient {
    if (!this.control) throw new Error('relay_control_not_active')
    return this.control
  }

  private requireEndpoint(): MobileRelayEndpoint {
    if (!this.endpoint) throw new Error('relay_endpoint_not_active')
    return this.endpoint
  }
}
