import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router'
import { api, type ArchiveListing, type Project, type PurgePlan, type PurgeReport } from '../api'
import { Callout, ListGroup, SectionHead, btn, useConfirm, usePrompt } from './ui'
import { useMe } from '../App'

/** Bytes as a short size ("3.2 MB"); small amounts read "under 1 KB". */
export function fmtBytes(n: number): string {
  if (n < 1024) return 'under 1 KB'
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 10 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`
}

const plural = (n: number, one: string, many = one + 's') => `${n} ${n === 1 ? one : many}`

function whatGoes(p: PurgePlan): string {
  const parts = [plural(p.conversations, 'conversation'), plural(p.messages, 'message'), plural(p.attachments, 'file'), plural(p.artifacts, 'artifact'), plural(p.runs, 'agent run')]
  if (p.agents) parts.push(plural(p.agents, 'agent'))
  if (p.sandboxes) parts.push(plural(p.sandboxes, 'sandbox'))
  return parts.join(', ')
}

/**
 * Archived chats and projects, with Restore and Delete permanently
 * (Settings, "Archive"). Archiving hides and can be undone; deleting here
 * removes the rows and the files from disk and cannot be undone.
 */
export default function ArchiveSection() {
  const qc = useQueryClient()
  const confirmDialog = useConfirm()
  const promptDialog = usePrompt()
  const me = useMe()
  const listing = useQuery({ queryKey: ['archive'], queryFn: () => api.get<ArchiveListing>('/api/archive') })
  const projects = useQuery({ queryKey: ['projects'], queryFn: () => api.get<Project[]>('/api/projects') })
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['archive'] })
    qc.invalidateQueries({ queryKey: ['projects'] })
    qc.invalidateQueries({ queryKey: ['convs'] })
  }
  const restoreConv = useMutation({ mutationFn: (id: string) => api.post(`/api/conversations/${id}/restore`, {}), onSuccess: refresh })
  const deleteConv = useMutation({ mutationFn: (id: string) => api.del<PurgeReport>(`/api/conversations/${id}/permanent`), onSuccess: refresh })
  const deleteAll = useMutation({ mutationFn: () => api.post<PurgeReport>('/api/archive/delete-all', {}), onSuccess: refresh })
  const archiveProject = useMutation({ mutationFn: (id: string) => api.post(`/api/projects/${id}/archive`, {}), onSuccess: refresh })
  const restoreProject = useMutation({ mutationFn: (id: string) => api.post(`/api/projects/${id}/restore`, {}), onSuccess: refresh })
  const deleteProject = useMutation({ mutationFn: (v: { id: string; name: string }) => api.post<PurgeReport>(`/api/projects/${v.id}/delete-permanently`, { name: v.name }), onSuccess: refresh })

  const d = listing.data
  const mine = (projects.data ?? []).filter((p) => p.owner_id === me.data?.id)
  const error = [restoreConv, deleteConv, deleteAll, archiveProject, restoreProject, deleteProject].map((m) => m.error as Error | null).find(Boolean)
  const done = deleteAll.data ?? deleteConv.data ?? deleteProject.data

  return (
    <>
      <section className="space-y-3">
        <SectionHead aside={d && d.conversations.length ? `${plural(d.conversations.length, 'chat')} · ${fmtBytes(d.all.bytes)}` : undefined}>archived chats</SectionHead>
        <p className="meta">Archiving a chat only hides it. Delete it here to remove its messages, files and artifacts for good and free the space.</p>
        {error && <Callout kind="error">{error.message}</Callout>}
        {done && (
          <Callout kind="note">
            Deleted: {whatGoes(done)}; {fmtBytes(done.bytes)} freed.
            {done.blobs_shared > 0 && ` ${plural(done.blobs_shared, 'file')} stayed because something else still uses the same bytes.`}
            {done.blobs_failed > 0 && ` ${plural(done.blobs_failed, 'file')} could not be removed from disk; see the server log.`}
          </Callout>
        )}
        <ListGroup empty={listing.isLoading ? 'Loading…' : 'No archived chats. Archive one from the sidebar and it waits here.'}>
          {(d?.conversations ?? []).map((c) => (
            <li key={c.id} className="px-3 py-2 flex flex-wrap items-center gap-x-3 gap-y-2">
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm">{c.title || 'New conversation'}</div>
                <div className="meta truncate">{c.project_name} · {plural(c.messages, 'message')} · {fmtBytes(c.bytes)}</div>
              </div>
              <button onClick={() => restoreConv.mutate(c.id)} disabled={restoreConv.isPending} className={`${btn.secondarySm} min-h-9`}>Restore</button>
              {c.can_delete && (
                <button
                  onClick={async () => {
                    if (await confirmDialog({ title: 'Delete this chat permanently?', body: `"${c.title || 'New conversation'}": ${plural(c.messages, 'message')} and its files and artifacts, about ${fmtBytes(c.bytes)}. This cannot be undone.`, confirm: 'Delete permanently', danger: true })) deleteConv.mutate(c.id)
                  }}
                  disabled={deleteConv.isPending}
                  className={btn.danger}
                >
                  Delete permanently
                </button>
              )}
            </li>
          ))}
        </ListGroup>
        {d && d.conversations.length > 1 && (
          <button
            onClick={async () => {
              if (await confirmDialog({ title: 'Delete all archived chats?', body: `${plural(d.all.conversations, 'chat')}: ${plural(d.all.messages, 'message')}, ${plural(d.all.attachments, 'file')} and ${plural(d.all.artifacts, 'artifact')}, about ${fmtBytes(d.all.bytes)}. This cannot be undone.`, confirm: `Delete ${d.all.conversations} chats`, danger: true })) deleteAll.mutate()
            }}
            disabled={deleteAll.isPending}
            className={btn.danger}
          >
            Delete all archived ({d.all.conversations}, {fmtBytes(d.all.bytes)})
          </button>
        )}
      </section>

      <section className="space-y-3">
        <SectionHead>projects</SectionHead>
        <p className="meta">Archiving a project hides it and its chats; restore brings them back. Only an archived project can be deleted permanently, with everything in it.</p>
        <ListGroup empty="You own no projects.">
          {mine.map((p) => (
            <li key={p.id} className="px-3 py-2 flex flex-wrap items-center gap-x-3 gap-y-2">
              <div className="min-w-0 flex-1">
                <div className="truncate text-sm">{p.name}</div>
                <div className="meta">{p.kind} project</div>
              </div>
              <button
                onClick={async () => {
                  if (await confirmDialog({ title: `Archive ${p.name}?`, body: 'It and its chats leave your lists. You can restore it here, and delete it permanently only after it is archived.', confirm: 'Archive project' })) archiveProject.mutate(p.id)
                }}
                disabled={archiveProject.isPending}
                className={`${btn.secondarySm} min-h-9`}
              >
                Archive project
              </button>
            </li>
          ))}
        </ListGroup>
        {(d?.projects ?? []).length > 0 && (
          <>
            <h3 className="meta">Archived projects</h3>
            <ListGroup empty="">
              {(d?.projects ?? []).map((p) => (
                <li key={p.id} className="px-3 py-2 flex flex-wrap items-center gap-x-3 gap-y-2">
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm">{p.name}</div>
                    <div className="meta">{whatGoes(p)} · {fmtBytes(p.bytes)}</div>
                  </div>
                  <button onClick={() => restoreProject.mutate(p.id)} disabled={restoreProject.isPending} className={`${btn.secondarySm} min-h-9`}>Restore</button>
                  <button
                    onClick={async () => {
                      const typed = await promptDialog({
                        title: `Delete ${p.name} permanently?`,
                        body: `This removes ${whatGoes(p)}, its files and its design system, about ${fmtBytes(p.bytes)}. It cannot be undone. Type the project name to confirm.`,
                        label: 'Project name',
                        confirm: 'Delete project permanently',
                        danger: true,
                        match: p.name,
                      })
                      if (typed === p.name) deleteProject.mutate({ id: p.id, name: p.name })
                    }}
                    disabled={deleteProject.isPending}
                    className={btn.danger}
                  >
                    Delete permanently
                  </button>
                </li>
              ))}
            </ListGroup>
          </>
        )}
        <p className="meta">Chats archived inside an archived project go with it. <Link to="/memory" className="underline">Memory</Link> kept from them stays.</p>
      </section>
    </>
  )
}
