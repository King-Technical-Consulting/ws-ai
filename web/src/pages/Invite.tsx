import { useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router'
import { useQueryClient } from '@tanstack/react-query'
import { api } from '../api'
import { registerPasskey } from '../lib/passkey'
import { PageTitle, Callout, WsMark, btn, input } from '../components/ui'

export default function Invite() {
  const { token = '' } = useParams()
  const nav = useNavigate()
  const qc = useQueryClient()
  const [email, setEmail] = useState<string | null>(null)
  const [name, setName] = useState('')
  const [err, setErr] = useState<string | null>(null)
  const [step, setStep] = useState<'peek' | 'accept' | 'passkey' | 'done'>('peek')

  useEffect(() => {
    api
      .post<{ email: string }>('/api/auth/invite/peek', { token })
      .then((r) => {
        setEmail(r.email)
        setStep('accept')
      })
      .catch(() => setErr('This invite link is invalid or has expired.'))
  }, [token])

  async function accept(e: React.FormEvent) {
    e.preventDefault()
    setErr(null)
    try {
      await api.post('/api/auth/invite/accept', { token, display_name: name })
      await qc.invalidateQueries({ queryKey: ['me'] })
      setStep('passkey')
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'Could not accept the invite.')
    }
  }

  async function addPasskey() {
    setErr(null)
    try {
      await registerPasskey('This device')
      setStep('done')
      setTimeout(() => nav('/', { replace: true }), 600)
    } catch (e) {
      setErr(e instanceof Error ? e.message : 'Passkey setup failed.')
    }
  }

  return (
    <div className="h-full grid place-items-center p-6">
      <div className="w-full max-w-sm space-y-5">
        <WsMark size={40} className="text-fg" />
        <PageTitle>Welcome to ws</PageTitle>
        {err && <Callout kind="error">{err}</Callout>}

        {step === 'peek' && !err && <p className="tagline text-fg-3">Checking your invite…</p>}

        {step === 'accept' && (
          <form onSubmit={accept} className="space-y-3">
            <p className="text-sm text-fg-2">
              You were invited as <span className="text-fg">{email}</span>. What should we call you?
            </p>
            <input value={name} onChange={(e) => setName(e.target.value)} placeholder="Your name" className={input} />
            <button className={`${btn.primary} w-full`}>Create account</button>
          </form>
        )}

        {step === 'passkey' && (
          <div className="space-y-3">
            <p className="text-sm text-fg-2">Account created. Add a passkey so you can sign in without email next time. You can also do this later in Settings.</p>
            <button onClick={addPasskey} className={`${btn.primary} w-full`}>Add a passkey</button>
            <button onClick={() => nav('/', { replace: true })} className={`${btn.secondary} w-full`}>Skip for now</button>
          </div>
        )}

        {step === 'done' && <p className="tagline text-fg-3">Passkey saved. Taking you in…</p>}
      </div>
    </div>
  )
}
