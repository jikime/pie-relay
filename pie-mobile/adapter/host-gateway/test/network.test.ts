import { describe, expect, it } from 'vitest'
import { chooseAdvertisedHost } from '../src/network.ts'

describe('chooseAdvertisedHost', () => {
  it('uses an explicit host unchanged', () => {
    expect(chooseAdvertisedHost(' 192.168.0.21 ')).toBe('192.168.0.21')
  })
})
