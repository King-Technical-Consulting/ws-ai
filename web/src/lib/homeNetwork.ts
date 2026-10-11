// What to put in WS_HOME_NETWORKS for the address ws sees: the address itself, and for IPv6 its /64, since
// a device's IPv6 address changes within its network. Returns [] for something that is not an address.
export function homeNetworkHints(address: string): string[] {
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(address)) return [address]
  const p = v6Prefix64(address)
  return p ? [address, p] : []
}

export function v6Prefix64(address: string): string | null {
  if (!address.includes(':') || !/^[0-9a-fA-F:]+$/.test(address)) return null
  const [head, tail, extra] = address.split('::')
  if (extra !== undefined) return null
  const a = head ? head.split(':') : []
  const b = tail === undefined ? [] : tail ? tail.split(':') : []
  const fill = tail === undefined ? 0 : 8 - a.length - b.length
  if (fill < 0 || (tail === undefined && a.length !== 8)) return null
  const groups = [...a, ...Array(fill).fill('0'), ...b]
  if (groups.length !== 8 || groups.some((g) => g === '' || g.length > 4)) return null
  return `${groups.slice(0, 4).map((g) => g.replace(/^0+(?=.)/, '').toLowerCase()).join(':')}::/64`
}
