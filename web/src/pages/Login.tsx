import { useState } from 'react'
import { useNavigate, useLocation } from 'react-router'
import { startAuthentication } from '@simplewebauthn/browser'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api'
import { KeyRound, Mail } from 'lucide-react'
import { Divider, Callout, btn, input } from '../components/ui'

export default function Login() {
  const nav = useNavigate()
  const loc = useLocation()
  const qc = useQueryClient()
  const authCfg = useQuery({ queryKey: ['auth-config'], queryFn: () => api.get<{ passkeys: boolean }>('/api/auth/config'), staleTime: 300_000 })
  const passkeysOn = authCfg.data?.passkeys ?? true
  const [email, setEmail] = useState('')
  const [sent, setSent] = useState(false)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(new URLSearchParams(loc.search).get('error') ? 'That link is invalid or expired.' : null)
  const from = (loc.state as { from?: string } | null)?.from ?? '/'

  async function passkey() {
    setErr(null)
    setBusy(true)
    try {
      const { options, ceremony_id } = await api.post<{ options: { publicKey: unknown }; ceremony_id: string }>('/api/auth/passkey/login/begin')
      const assertion = await startAuthentication({ optionsJSON: options.publicKey as never })
      await api.postRaw(`/api/auth/passkey/login/finish?ceremony=${ceremony_id}`, JSON.stringify(assertion))
      await qc.invalidateQueries({ queryKey: ['me'] })
      nav(from, { replace: true })
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'Passkey sign-in failed.')
    } finally {
      setBusy(false)
    }
  }

  async function magic(e: React.FormEvent) {
    e.preventDefault()
    setErr(null)
    setBusy(true)
    try {
      await api.post('/api/auth/magic/send', { email })
      setSent(true)
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'Could not send the link.')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="h-full grid place-items-center p-6">
      <div className="w-full max-w-sm space-y-6">
        <div>
          <h1 className="wordmark">ws</h1>
          <p className="tagline text-fg-2 mt-1">Self-hosted AI workspace. Invite only.</p>
        </div>

        {passkeysOn ? (
          <>
            <button onClick={passkey} disabled={busy} className={`${btn.primary} w-full`}>
              <KeyRound size={18} /> Sign in with passkey
            </button>
            <Divider>or</Divider>
          </>
        ) : (
          <p className="meta">Passkeys are off on this host until it has a domain name over https.</p>
        )}

        {sent ? (
          <p className="text-sm text-fg-2">If that address is a member, a sign-in link is on its way. It expires in 15 minutes.</p>
        ) : (
          <form onSubmit={magic} className="space-y-2">
            <input type="email" required value={email} onChange={(e) => setEmail(e.target.value)} placeholder="you@example.com" className={input} />
            <button type="submit" disabled={busy} className={`${btn.secondary} w-full`}>
              <Mail size={18} /> Email me a link
            </button>
          </form>
        )}

        {err && <Callout kind="error">{err}</Callout>}
      </div>
    </div>
  )
}
