import assert from 'node:assert/strict'
import test from 'node:test'

import { usageEventFromResult } from './executor.mjs'

test('usageEventFromResult preserves per-model token and provider cost measurements', () => {
  const event = usageEventFromResult({
    type: 'result',
    subtype: 'success',
    uuid: 'result-1',
    session_id: 'session-1',
    total_cost_usd: 0.0123,
    usage: { input_tokens: 123, output_tokens: 45 },
    modelUsage: {
      'claude-sonnet-test': {
        inputTokens: 100,
        outputTokens: 40,
        cacheReadInputTokens: 20,
        cacheCreationInputTokens: 3,
        webSearchRequests: 1,
        costUSD: 0.0123,
        contextWindow: 200000,
        maxOutputTokens: 64000,
        canonicalModel: 'claude-sonnet-test-v1',
        provider: 'firstParty',
      },
    },
  }, { queryRunId: 'run-1', requestId: 'request-1', reportedAt: '2026-08-05T00:00:00.000Z' })

  assert.deepEqual(event, {
    type: 'usage',
    schemaVersion: 1,
    resultId: 'result-1',
    queryRunId: 'run-1',
    requestId: 'request-1',
    sessionId: 'session-1',
    subtype: 'success',
    reportedAt: '2026-08-05T00:00:00.000Z',
    totalCostUsd: 0.0123,
    usage: { input_tokens: 123, output_tokens: 45 },
    modelUsage: {
      'claude-sonnet-test': {
        inputTokens: 100,
        outputTokens: 40,
        cacheReadInputTokens: 20,
        cacheCreationInputTokens: 3,
        webSearchRequests: 1,
        costUSD: 0.0123,
        contextWindow: 200000,
        maxOutputTokens: 64000,
        canonicalModel: 'claude-sonnet-test-v1',
        provider: 'firstParty',
      },
    },
  })
})

test('usageEventFromResult rejects non-result events and clamps invalid numbers', () => {
  assert.equal(usageEventFromResult({ type: 'assistant' }), null)
  const event = usageEventFromResult({
    type: 'result', uuid: 'result-2', session_id: 'session-2', modelUsage: {
      model: { inputTokens: -1, outputTokens: Number.NaN, costUSD: Number.POSITIVE_INFINITY },
    },
  })
  assert.equal(event.modelUsage.model.inputTokens, 0)
  assert.equal(event.modelUsage.model.outputTokens, 0)
  assert.equal(event.modelUsage.model.costUSD, 0)
})
