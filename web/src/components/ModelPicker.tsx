import type { ModelsResponse } from '../api'
import { Brain } from 'lucide-react'
import clsx from 'clsx'

export function ModelPicker({
  value,
  onChange,
  models,
  reasoning,
  onReasoning,
}: {
  value: string
  onChange: (v: string) => void
  models?: ModelsResponse
  reasoning: boolean
  onReasoning: (v: boolean) => void
}) {
  const aliases = Object.keys(models?.aliases ?? { auto: null })
  const local = models?.models.filter((m) => m.local) ?? []
  const hosted = models?.models.filter((m) => !m.local) ?? []
  return (
    <div className="flex items-center gap-2">
      <button
        onClick={() => onReasoning(!reasoning)}
        aria-pressed={reasoning}
        className={clsx('p-1.5 rounded-md border', reasoning ? 'border-accent text-accent' : 'border-line text-fg-3 hover:text-fg')}
        title="Extended thinking"
        aria-label="Extended thinking"
      >
        <Brain size={14} />
      </button>
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="text-sm rounded-md border border-line bg-bg-2 px-2 py-1.5 max-w-[16rem] outline-none focus:border-accent"
        aria-label="Model"
      >
        <optgroup label="Routing">
          {aliases.map((a) => (
            <option key={a} value={a}>{a}</option>
          ))}
        </optgroup>
        {local.length > 0 && (
          <optgroup label="Local">
            {local.map((m) => (
              <option key={m.id} value={m.id} disabled={m.health === 'down'}>
                {m.display_name}{m.health === 'down' ? ' (down)' : ''}
              </option>
            ))}
          </optgroup>
        )}
        {hosted.length > 0 && (
          <optgroup label="Hosted">
            {hosted.map((m) => (
              <option key={m.id} value={m.id} disabled={m.health === 'down'}>
                {m.display_name}
              </option>
            ))}
          </optgroup>
        )}
      </select>
    </div>
  )
}
