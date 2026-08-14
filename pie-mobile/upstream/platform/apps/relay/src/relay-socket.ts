import type { WebSocket } from 'ws'

// Minimal transport surface so the connection logic is testable with a fake
// socket and adapts to a real `ws` WebSocket in production/integration tests.
export type RelaySocket = {
  send: (serialized: string) => void
  close: (code: number, reason: string) => void
  // Socket send-buffer depth in bytes; drives backpressure decisions.
  bufferedAmount: () => number
  onMessage: (handler: (data: string) => void) => void
  onClose: (handler: () => void) => void
}

export function createWsRelaySocket(ws: WebSocket): RelaySocket {
  return {
    send: (serialized) => {
      // Guard against sending on a closing socket; forwarding is best-effort.
      if (ws.readyState === ws.OPEN) {
        ws.send(serialized)
      }
    },
    close: (code, reason) => ws.close(code, reason),
    bufferedAmount: () => ws.bufferedAmount,
    onMessage: (handler) => {
      ws.on('message', (data) => handler(typeof data === 'string' ? data : data.toString()))
    },
    onClose: (handler) => {
      ws.on('close', handler)
    }
  }
}
