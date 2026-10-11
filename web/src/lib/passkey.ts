import { startAuthentication, startRegistration } from '@simplewebauthn/browser'
import { api } from '../api'

/** Registers a new passkey for the signed-in user. */
export async function registerPasskey(name: string) {
  const { options, ceremony_id } = await api.post<{ options: { publicKey: unknown }; ceremony_id: string }>(
    '/api/auth/passkey/register/begin',
  )
  const attestation = await startRegistration({ optionsJSON: options.publicKey as never })
  await api.postRaw(
    `/api/auth/passkey/register/finish?ceremony=${ceremony_id}&name=${encodeURIComponent(name)}`,
    JSON.stringify(attestation),
  )
}

/** Confirms a sensitive action with a passkey: one assertion on this session unlocks the gated actions for the step-up window. */
export async function stepUp() {
  const { options, ceremony_id } = await api.post<{ options: { publicKey: unknown }; ceremony_id: string }>('/api/auth/stepup/begin')
  const assertion = await startAuthentication({ optionsJSON: options.publicKey as never })
  await api.postRaw(`/api/auth/stepup/finish?ceremony=${ceremony_id}`, JSON.stringify(assertion))
}
