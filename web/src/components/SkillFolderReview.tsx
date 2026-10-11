import type { SkillFolderImport } from '../api'
import { Callout } from './ui'

/** What an uploaded skill folder holds, with every finding. Used before an import fills the form and before a skill is approved for an agent. */
export function SkillFolderReview({ folder }: { folder: SkillFolderImport }) {
  return (
    <div className="rounded-lg border border-line p-3 space-y-2" aria-label="Skill folder contents">
      <p className="text-sm">
        {folder.usable_anywhere
          ? 'No scripts: this skill is instructions only and works in any agent.'
          : 'This skill carries scripts. They are never run by the import; an agent can run them only inside a code project\'s sandbox, through bash, under its tool policy.'}
      </p>
      {folder.refused_secrets.length > 0 && (
        <Callout kind="error">Credentials named in the skill, not stored: <span className="font-mono text-xs">{folder.refused_secrets.join(', ')}</span></Callout>
      )}
      {folder.findings.length > 0 && (
        <ul className="list-disc pl-4 text-xs text-fg-2">{folder.findings.map((f) => <li key={f}>{f}</li>)}</ul>
      )}
      {folder.files.length === 0 ? (
        <p className="meta">The folder holds only SKILL.md.</p>
      ) : (
        <ul className="divide-y divide-line text-xs">
          {folder.files.map((f) => (
            <li key={f.path} className="py-1.5">
              <span className="font-mono break-all">{f.path}</span>
              <span className="meta"> · {f.kind}{f.interpreter ? ` (${f.interpreter})` : ''} · {f.size.toLocaleString()} bytes{f.refused ? ' · not stored' : ''}</span>
              {f.findings.length > 0 && <ul className="list-disc pl-4 text-danger">{f.findings.map((x) => <li key={x}>{x}</li>)}</ul>}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
