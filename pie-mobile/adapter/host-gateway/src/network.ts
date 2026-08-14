import { networkInterfaces } from 'node:os'

type AddressCandidate = {
  address: string
  priority: number
}

function priorityFor(name: string, address: string): number {
  if (/tailscale/i.test(name) || address.startsWith('100.')) {
    return 0
  }
  if (/wi-?fi|wlan|en0/i.test(name)) {
    return 1
  }
  if (/ethernet|en\d+/i.test(name)) {
    return 2
  }
  return 3
}

export function listAdvertisableIpv4(): string[] {
  const candidates: AddressCandidate[] = []
  for (const [name, addresses] of Object.entries(networkInterfaces())) {
    for (const item of addresses ?? []) {
      if (item.family !== 'IPv4' || item.internal) {
        continue
      }
      candidates.push({ address: item.address, priority: priorityFor(name, item.address) })
    }
  }
  return candidates
    .sort((a, b) => a.priority - b.priority || a.address.localeCompare(b.address))
    .map((candidate) => candidate.address)
}

export function chooseAdvertisedHost(explicit?: string): string {
  if (explicit?.trim()) {
    return explicit.trim()
  }
  const [first] = listAdvertisableIpv4()
  if (!first) {
    throw new Error('No non-loopback IPv4 address found; pass --advertise-host explicitly')
  }
  return first
}
