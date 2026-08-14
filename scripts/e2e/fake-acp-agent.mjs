#!/usr/bin/env node

// Deterministic ACP v2 agent used only by the local Docker integration test.
// It exercises the real clientd -> Relay -> participant path without copying
// a developer's Claude credentials into an ephemeral Executor container.

import { createInterface } from 'node:readline'

const input = createInterface({ input: process.stdin })
const send = (value) => process.stdout.write(`${JSON.stringify(value)}\n`)
let sessionSequence = 0

for await (const line of input) {
  const trimmed = line.trim()
  if (!trimmed) continue
  const message = JSON.parse(trimmed)

  if (message.method === 'initialize') {
    send({
      jsonrpc: '2.0',
      id: message.id,
      result: {
        protocolVersion: 2,
        agentCapabilities: { loadSession: false, promptCapabilities: { image: true } },
      },
    })
    continue
  }

  if (message.method === 'session/new') {
    sessionSequence += 1
    send({
      jsonrpc: '2.0',
      id: message.id,
      result: { sessionId: `pie-canvas-docker-acp-${sessionSequence}` },
    })
    continue
  }

  if (message.method === 'session/prompt') {
    const sessionId = message.params?.sessionId || `pie-canvas-docker-acp-${sessionSequence || 1}`
    const blocks = Array.isArray(message.params?.prompt) ? message.params.prompt : []
    const prompt = blocks
      .filter((block) => block?.type === 'text' && typeof block.text === 'string')
      .map((block) => block.text)
      .join('\n')
    const marker = prompt.includes('PIE_CANVAS_ACP_DOCKER_E2E')
      ? 'PIE_CANVAS_ACP_DOCKER_E2E_OK'
      : 'Pie Canvas 로컬 ACP 테스트 Agent 응답'

    send({
      jsonrpc: '2.0',
      method: 'session/update',
      params: {
        sessionId,
        update: {
          sessionUpdate: 'agent_message_chunk',
          content: { type: 'text', text: marker },
        },
      },
    })
    send({ jsonrpc: '2.0', id: message.id, result: { stopReason: 'end_turn' } })
    continue
  }

  if (Object.hasOwn(message, 'id')) {
    send({
      jsonrpc: '2.0',
      id: message.id,
      error: { code: -32601, message: `Method not found: ${message.method}` },
    })
  }
}
