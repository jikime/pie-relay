#!/usr/bin/env node
import { resolve } from 'node:path'
import { homedir } from 'node:os'
import { createInterface } from 'node:readline'
import { HostGateway } from './host-gateway.ts'
import type { MobileConnectionMode } from './host-gateway.ts'
import { chooseAdvertisedHost, listAdvertisableIpv4 } from './network.ts'
import { resolveRelayUrl } from './relay-url.ts'

type CliOptions = {
  advertiseHost?: string
  listenHost?: string
  port?: number
  dataDir?: string
  cwd?: string
  shell?: string
  rotatePairing?: boolean
  terminalQr?: boolean
  connectionMode?: MobileConnectionMode
  relayUrl?: string
  relayToken?: string
}

function parseArgs(args: string[]): CliOptions {
  const options: CliOptions = {}
  for (let index = 0; index < args.length; index++) {
    const arg = args[index]
    const next = args[index + 1]
    switch (arg) {
      case '--':
        break
      case '--advertise-host':
        options.advertiseHost = next
        index++
        break
      case '--listen-host':
        options.listenHost = next
        index++
        break
      case '--port':
        options.port = Number(next)
        index++
        break
      case '--data-dir':
        options.dataDir = next
        index++
        break
      case '--cwd':
        options.cwd = next
        index++
        break
      case '--shell':
        options.shell = next
        index++
        break
      case '--rotate-pairing':
        options.rotatePairing = true
        break
      case '--no-terminal-qr':
        options.terminalQr = false
        break
      case '--connection-mode':
        if (next !== 'automatic' && next !== 'local-only' && next !== 'relay-only') {
          throw new Error(`Invalid connection mode: ${next}`)
        }
        options.connectionMode = next
        index++
        break
      case '--relay-url':
        options.relayUrl = next
        index++
        break
      case '--relay-token':
        options.relayToken = next
        index++
        break
      case '--list-addresses':
        console.log(listAdvertisableIpv4().join('\n'))
        process.exit(0)
      default:
        throw new Error(`Unknown argument: ${arg}`)
    }
  }
  return options
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2))
  const relayUrl = resolveRelayUrl(process.env, args.relayUrl)
  const relayToken = args.relayToken ?? process.env.PIE_RELAY_TOKEN
  const connectionMode = args.connectionMode ?? (relayToken ? 'automatic' : 'local-only')
  const emitDevices = (devices: ReturnType<HostGateway['listDevices']>) => {
    console.log(JSON.stringify({ type: 'mobile-host-devices', devices }))
  }
  const gateway = new HostGateway({
    dataDir: resolve(args.dataDir ?? `${homedir()}/.pie-relay/mobile`),
    cwd: resolve(args.cwd ?? process.cwd()),
    advertiseHost: chooseAdvertisedHost(args.advertiseHost),
    listenHost: args.listenHost,
    port: args.port,
    shell: args.shell,
    rotatePairing: args.rotatePairing,
    connectionMode,
    relayUrl,
    relayToken,
    onDevicesChanged: emitDevices,
    onRelayStatus: (status, detail) => {
      console.log(JSON.stringify({ type: 'mobile-relay-status', status, ...(detail ? { detail } : {}) }))
    }
  })
  const ready = await gateway.start()

  console.log(`[mobile-host] listening on ${ready.endpoint}`)
  console.log(`[mobile-host] pairing metadata: ${ready.pairingFile}`)
  console.log(
    JSON.stringify({
      type: 'mobile-host-ready',
      endpoint: ready.endpoint,
      pairingUrl: ready.pairingUrl,
      deviceId: ready.deviceId,
      port: ready.port,
      connectionMode: ready.connectionMode,
      ...(ready.relayEndpoint ? { relayEndpoint: ready.relayEndpoint } : {})
    })
  )
  if (args.terminalQr !== false) {
    console.log(ready.qrText)
    console.log(ready.pairingUrl)
  }

  emitDevices(gateway.listDevices())

  // The desktop parent manages the live registry over this private stdin
  // channel. Revocation therefore updates the exact in-memory Orca registry
  // used for authentication and immediately terminates that device's sockets.
  const commands = createInterface({ input: process.stdin, terminal: false })
  commands.on('line', (line) => {
    try {
      const command = JSON.parse(line) as { type?: string; deviceId?: string }
      if (command.type === 'list-devices') {
        emitDevices(gateway.listDevices())
      } else if (command.type === 'revoke-device' && typeof command.deviceId === 'string') {
        const revoked = gateway.revokeDevice(command.deviceId)
        console.log(
          JSON.stringify({
            type: 'mobile-host-device-revoked',
            deviceId: command.deviceId,
            revoked
          })
        )
        emitDevices(gateway.listDevices())
      }
    } catch {
      console.error('[mobile-host] ignored malformed desktop control message')
    }
  })

  // Tauri normally stops the child during RunEvent::Exit. A crash or forced
  // kill skips that hook, though, so also watch the owning desktop PID and
  // terminate instead of leaving a LAN listener orphaned in the background.
  let stopping = false
  let ownerWatch: ReturnType<typeof setInterval> | null = null
  const stop = async () => {
    if (stopping) {
      return
    }
    stopping = true
    if (ownerWatch) {
      clearInterval(ownerWatch)
    }
    commands.close()
    await gateway.stop()
    process.exit(0)
  }
  const ownerPid = Number(process.env.PIE_RELAY_APP_PID ?? process.env.CLI_RELAY_APP_PID)
  if (Number.isInteger(ownerPid) && ownerPid > 0) {
    const timer = setInterval(() => {
      try {
        process.kill(ownerPid, 0)
      } catch {
        void stop()
      }
    }, 2_000)
    ownerWatch = timer
    ;(timer as { unref?: () => void }).unref?.()
  }
  process.on('SIGINT', () => void stop())
  process.on('SIGTERM', () => void stop())
}

main().catch((error) => {
  console.error(error instanceof Error ? error.stack : String(error))
  process.exit(1)
})
