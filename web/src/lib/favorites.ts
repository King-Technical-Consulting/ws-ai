import { useMemo } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api'

export type FavoriteKind = 'message' | 'artifact' | 'media'
export type Favorite = {
  id: string
  kind: FavoriteKind
  ref_id: string
  title: string
  snippet: string
  conversation_id: string | null
  project_id: string | null
  created_at: string
}

/** The member's favorites, and a toggle for the star on a message, artifact or media item. */
export function useFavorites() {
  const qc = useQueryClient()
  const q = useQuery({ queryKey: ['favorites'], queryFn: () => api.get<{ favorites: Favorite[] }>('/api/favorites') })
  const starred = useMemo(() => new Set((q.data?.favorites ?? []).map((f) => `${f.kind}:${f.ref_id}`)), [q.data])
  const toggle = useMutation({
    mutationFn: ({ kind, ref, on }: { kind: FavoriteKind; ref: string; on: boolean }) =>
      on ? api.post('/api/favorites', { kind, ref_id: ref }) : api.del(`/api/favorites/${kind}/${ref}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['favorites'] }),
  })
  return {
    favorites: q.data?.favorites ?? [],
    isStarred: (kind: FavoriteKind, ref: string) => starred.has(`${kind}:${ref}`),
    toggle: (kind: FavoriteKind, ref: string) => toggle.mutate({ kind, ref, on: !starred.has(`${kind}:${ref}`) }),
  }
}
