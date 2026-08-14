import { mkdirSync } from 'node:fs'
import { join } from 'node:path'
import QRCode from 'qrcode'
import { DeviceRegistry } from '../../../upstream/src/main/runtime/device-registry.ts'
import { loadOrCreateE2EEKeypair } from '../../../upstream/src/main/runtime/e2ee-keypair.ts'
import { MobileSocketWiring } from '../../../upstream/src/main/runtime/rpc/mobile-socket-wiring.ts'
import { WebSocketTransport } from '../../../upstream/src/main/runtime/rpc/ws-transport.ts'
import {
  readWsFallbackPort,
  writeWsFallbackPort
} from '../../../upstream/src/main/runtime/rpc/ws-fallback-port-store.ts'
import { encodePairingOffer } from '../../../upstream/src/shared/pairing.ts'
import { writeSecureJsonFile } from '../../../upstream/src/shared/secure-file.ts'
import { MobileRpcHandler } from './rpc-handler.ts'
import { TerminalSession } from './terminal-session.ts'
import { RelayGateway, type RelayGatewayStatus } from './relay-gateway.ts'

export type MobileConnectionMode = 'automatic' | 'local-only' | 'relay-only'

export type HostGatewayOptions = {
  dataDir: string
  cwd: string
  advertiseHost: string
  listenHost?: string
  port?: number
  shell?: string
  rotatePairing?: boolean
  connectionMode?: MobileConnectionMode
  relayUrl?: string
  relayToken?: string
  onDevicesChanged?: (devices: PairedMobileDevice[]) => void
  onRelayStatus?: (status: RelayGatewayStatus, detail?: string) => void
}

export type PairedMobileDevice = {
  deviceId: string
  name: string
  pairedAt: number
  lastSeenAt: number
}

export type HostGatewayReady = {
  endpoint: string
  pairingUrl: string
  pairingFile: string
  qrText: string
  deviceId: string
  port: number
  connectionMode: MobileConnectionMode
  relayEndpoint?: string
}

export class HostGateway {
  private readonly options: HostGatewayOptions
  private readonly deviceRegistry: DeviceRegistry
  private readonly transport: WebSocketTransport
  private readonly terminal: TerminalSession
  private readonly rpc: MobileRpcHandler
  private readonly wiring: MobileSocketWiring
  private readonly relay: RelayGateway | null
  private readonly preferredPort: number

  constructor(options: HostGatewayOptions) {
    this.options = options
    mkdirSync(options.dataDir, { recursive: true, mode: 0o700 })
    this.deviceRegistry = new DeviceRegistry(options.dataDir)
    const keypair = loadOrCreateE2EEKeypair(options.dataDir)
    this.preferredPort = options.port ?? 6768
    this.transport = new WebSocketTransport({
      host: options.listenHost ?? '0.0.0.0',
      port: this.preferredPort,
      // Keep Orca's assigned fallback stable across restarts. The mobile app
      // stores the complete endpoint, so choosing a new random port would make
      // an already-paired phone unable to reconnect whenever 6768 is occupied.
      ...(this.preferredPort !== 0
        ? { fallbackPort: readWsFallbackPort(options.dataDir) }
        : {})
    })
    this.terminal = new TerminalSession({ cwd: options.cwd, shell: options.shell })
    let rpc!: MobileRpcHandler
    this.wiring = new MobileSocketWiring({
      deviceRegistry: this.deviceRegistry,
      e2eeKeypair: keypair,
      onText: (socket, plaintext, reply) => this.rpc.handle(socket, plaintext, reply),
      onBinary: () => {
        // Binary terminal multiplex is deliberately not advertised until the
        // cli-relay adapter implements Orca's exact terminal framing contract.
      },
      onClose: (socket) => {
        if (socket) {
          this.rpc.closeConnection(socket.connectionId)
        }
      },
      onReady: () => this.options.onDevicesChanged?.(this.listDevices())
    })
    const connectionMode = options.connectionMode ?? 'local-only'
    this.relay =
      connectionMode !== 'local-only' && options.relayUrl && options.relayToken
        ? new RelayGateway({
            relayUrl: options.relayUrl,
            relayToken: options.relayToken,
            keypair,
            mobileSocketWiring: this.wiring,
            onStatus: options.onRelayStatus
          })
        : null
    rpc = new MobileRpcHandler({ cwd: options.cwd, terminal: this.terminal, relay: this.relay ?? undefined })
    this.rpc = rpc
    this.wiring.attachTransport(this.transport)
  }

  async start(): Promise<HostGatewayReady> {
    await this.transport.start()
    if (this.preferredPort !== 0 && this.transport.resolvedPort !== this.preferredPort) {
      writeWsFallbackPort(this.options.dataDir, this.transport.resolvedPort)
    }
    const device = this.options.rotatePairing
      ? this.deviceRegistry.rotatePendingDevice('Pie Relay mobile', 'mobile')
      : this.deviceRegistry.getOrCreatePendingDevice('Pie Relay mobile', 'mobile')
    const connectionMode = this.options.connectionMode ?? 'local-only'
    this.deviceRegistry.setMobilePairingConnectionMode(
      device.deviceId,
      connectionMode === 'local-only' ? 'local-only' : 'automatic'
    )
    const endpoint = `ws://${this.options.advertiseHost}:${this.transport.resolvedPort}`
    const keypair = loadOrCreateE2EEKeypair(this.options.dataDir)
    let relay
    if (connectionMode !== 'local-only') {
      if (!this.relay) {
        throw new Error('Pie Relay URL and host token are required for Relay mode')
      }
      await this.relay.start()
      relay = await this.relay.createPairingRelay(device.deviceId)
    }
    const pairingUrl = encodePairingOffer({
      v: 2,
      endpoint,
      deviceToken: device.token,
      publicKeyB64: keypair.publicKeyB64,
      scope: 'mobile',
      connectionMode,
      ...(relay ? { relay } : {})
    })
    const pairingFile = join(this.options.dataDir, 'pairing.json')
    writeSecureJsonFile(pairingFile, {
      v: 1,
      endpoint,
      pairingUrl,
      deviceId: device.deviceId,
      port: this.transport.resolvedPort,
      createdAt: Date.now()
    })
    const qrText = await QRCode.toString(pairingUrl, { type: 'terminal', small: true })
    return {
      endpoint,
      pairingUrl,
      pairingFile,
      qrText,
      deviceId: device.deviceId,
      port: this.transport.resolvedPort,
      connectionMode,
      ...(relay ? { relayEndpoint: relay.cellUrl } : {})
    }
  }

  async stop(): Promise<void> {
    this.terminal.close()
    await this.relay?.stop()
    await this.transport.stop()
  }

  listDevices(): PairedMobileDevice[] {
    return this.deviceRegistry
      .listDevices()
      .filter((device) => device.scope === 'mobile' && device.lastSeenAt > 0)
      .map(({ deviceId, name, pairedAt, lastSeenAt }) => ({
        deviceId,
        name,
        pairedAt,
        lastSeenAt
      }))
  }

  revokeDevice(deviceId: string): boolean {
    const device = this.deviceRegistry.getDevice(deviceId)
    if (!device || device.scope !== 'mobile' || !this.deviceRegistry.removeDevice(deviceId)) {
      return false
    }
    this.wiring.terminateDeviceConnections(device.token)
    void this.relay?.revokeDevice(deviceId).catch(() => {})
    this.options.onDevicesChanged?.(this.listDevices())
    return true
  }
}
