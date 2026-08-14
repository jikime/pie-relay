import { createAPIHandler } from './api-handler.mjs'
import { loadConfig } from './config.mjs'
import { PieManagerClient } from './pie-manager-client.mjs'

const runtimeKey = Symbol.for('pielab.third-party-web-chat.runtime')

export function getAPIHandler() {
  return getRuntime().handler
}

export function getPublicConfig() {
  const { config } = getRuntime()
  return Object.freeze({ registrationEnabled: config.registrationEnabled })
}

function getRuntime() {
  if (!globalThis[runtimeKey]) {
    const config = loadConfig()
    const pieClient = new PieManagerClient(config)
    globalThis[runtimeKey] = Object.freeze({
      config,
      handler: createAPIHandler({ config, pieClient }),
    })
  }
  return globalThis[runtimeKey]
}
