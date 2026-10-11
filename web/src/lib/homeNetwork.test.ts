import { describe, expect, it } from 'vitest'
import { homeNetworkHints, v6Prefix64 } from './homeNetwork'

describe('home network hints', () => {
  it('gives an IPv4 address as it is', () => {
    expect(homeNetworkHints('203.0.113.7')).toEqual(['203.0.113.7'])
  })
  it('gives an IPv6 address and its /64', () => {
    expect(homeNetworkHints('2001:db8:abcd:12::9')).toEqual(['2001:db8:abcd:12::9', '2001:db8:abcd:12::/64'])
    expect(v6Prefix64('2001:0db8:0000:0001:aaaa:bbbb:cccc:dddd')).toBe('2001:db8:0:1::/64')
    expect(v6Prefix64('2001:db8::1')).toBe('2001:db8:0:0::/64')
    expect(v6Prefix64('::1')).toBe('0:0:0:0::/64')
  })
  it('gives nothing for what is not an address', () => {
    expect(homeNetworkHints('')).toEqual([])
    expect(homeNetworkHints('hello')).toEqual([])
    expect(v6Prefix64('1:2:3')).toBeNull()
    expect(v6Prefix64('1::2::3')).toBeNull()
  })
})
