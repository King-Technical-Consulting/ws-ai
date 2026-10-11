import { useState } from 'react'
import { Link } from 'react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api'
import { SectionHead, PageTitle, Callout, ListGroup, btn, inputSm } from '../components/ui'
import { useFavorites, type Favorite } from '../lib/favorites'

type Prompt = { id: string; name: string; text: string }

const KIND: Record<Favorite['kind'], string> = { message: 'answer', artifact: 'artifact', media: 'media' }

function target(f: Favorite): string {
  if (f.kind === 'media') return '/media'
  return f.conversation_id ? `/c/${f.conversation_id}` : '/'
}

/** Starred answers, artifacts and media across projects, and the saved prompts the "/" list offers. */
export default function Favorites() {
  const qc = useQueryClient()
  const fav = useFavorites()
  const prompts = useQuery({ queryKey: ['saved-prompts'], queryFn: () => api.get<{ prompts: Prompt[] }>('/api/saved-prompts') })
  const [name, setName] = useState('')
  const [text, setText] = useState('')
  const [error, setError] = useState('')
  const refresh = () => qc.invalidateQueries({ queryKey: ['saved-prompts'] })
  const fail = (e: unknown) => setError(e instanceof Error ? e.message : 'Something went wrong.')
  const add = useMutation({
    mutationFn: () => api.post('/api/saved-prompts', { name, text }),
    onSuccess: () => {
      setName('')
      setText('')
      setError('')
      refresh()
    },
    onError: fail,
  })
  const del = useMutation({ mutationFn: (id: string) => api.del(`/api/saved-prompts/${id}`), onSuccess: refresh, onError: fail })

  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto max-w-2xl p-6 space-y-10">
        <div>
          <PageTitle>Favorites</PageTitle>
          <p className="tagline mt-1">Answers, artifacts and media you starred, from every project, and prompts you reuse.</p>
        </div>
        {error && <Callout kind="error">{error}</Callout>}

        <section className="space-y-3">
          <SectionHead aside={`${fav.favorites.length}`}>starred</SectionHead>
          <ListGroup empty="Nothing starred yet. Use the star under an answer, in an artifact's header, or on a media item.">
            {fav.favorites.map((f) => (
              <li key={f.id} className="flex items-start justify-between gap-3 px-3 py-2">
                <div className="min-w-0">
                  <Link to={target(f)} className="block truncate text-accent hover:text-accent-hover">{f.title || KIND[f.kind]}</Link>
                  {f.snippet && <p className="reading-tight line-clamp-2 text-fg-2">{f.snippet}</p>}
                  <p className="meta">{KIND[f.kind]} · {new Date(f.created_at).toLocaleDateString()}</p>
                </div>
                <button className={btn.danger} onClick={() => fav.toggle(f.kind, f.ref_id)} aria-label={`Remove favorite: ${f.title || KIND[f.kind]}`}>Remove</button>
              </li>
            ))}
          </ListGroup>
        </section>

        <section className="space-y-3">
          <SectionHead aside={`${prompts.data?.prompts.length ?? 0} of 50`}>saved prompts</SectionHead>
          <p className="meta">Type / in the message box to insert one.</p>
          <ListGroup empty="No saved prompts yet.">
            {(prompts.data?.prompts ?? []).map((p) => (
              <li key={p.id} className="flex items-start justify-between gap-3 px-3 py-2">
                <div className="min-w-0">
                  <p className="truncate">{p.name}</p>
                  <p className="reading-tight line-clamp-2 text-fg-2 whitespace-pre-wrap">{p.text}</p>
                </div>
                <button className={btn.danger} onClick={() => del.mutate(p.id)} aria-label={`Delete saved prompt: ${p.name}`}>Delete</button>
              </li>
            ))}
          </ListGroup>
          <form
            className="space-y-2 text-sm"
            onSubmit={(e) => {
              e.preventDefault()
              if (name.trim() && text.trim()) add.mutate()
            }}
          >
            <div>
              <label htmlFor="sp-name" className="meta block">Name</label>
              <input id="sp-name" value={name} onChange={(e) => setName(e.target.value)} className={inputSm} maxLength={60} placeholder="Weekly review" />
            </div>
            <div>
              <label htmlFor="sp-text" className="meta block">Prompt</label>
              <textarea id="sp-text" value={text} onChange={(e) => setText(e.target.value)} rows={4} className={inputSm + ' reading'} />
            </div>
            <button type="submit" className={btn.secondarySm} disabled={!name.trim() || !text.trim() || add.isPending}>Save prompt</button>
          </form>
        </section>
      </div>
    </div>
  )
}
