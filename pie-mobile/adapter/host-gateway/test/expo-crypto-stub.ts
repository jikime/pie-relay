import { randomFillSync } from 'node:crypto'
export function getRandomBytes(length: number): Uint8Array {
  const bytes = new Uint8Array(length)
  randomFillSync(bytes)
  return bytes
}
