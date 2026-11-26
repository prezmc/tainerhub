import { Server } from 'lucide-react'
import type { Host, Role } from './api'
import { scopeProblem, toggleHost, type Scope } from './hostscope'
import { Badge, StatusDot } from './ui'

/** "All hosts" or "Only some hosts", with the hosts to tick. */
export function HostScopePicker({ value, onChange, role, hosts }: { value: Scope; onChange: (s: Scope) => void; role: Role; hosts: Host[] }) {
  const problem = scopeProblem(role, value, hosts)
  const option = (scoped: boolean, title: string, help: string) => (
    <label className="flex cursor-pointer items-start gap-2.5 rounded-[3px] p-2.5 ring-1 ring-inset ring-line has-[:checked]:bg-lime-400/[0.07] has-[:checked]:ring-lime-400/50">
      <input type="radio" name="host-scope" checked={value.scoped === scoped} onChange={() => onChange({ ...value, scoped })} className="mt-1 accent-lime-400" />
      <span>
        <span className="block text-sm font-semibold text-white">{title}</span>
        <span className="block text-xs text-ink-100/60">{help}</span>
      </span>
    </label>
  )
  return (
    <fieldset className="space-y-2">
      <legend className="mb-1.5 text-sm font-medium text-ink-100/80">Hosts</legend>
      {option(false, 'All hosts', 'Including hosts added later.')}
      {option(true, 'Only some hosts', role === 'admin' ? "Other hosts are hidden. A host admin can't add hosts, manage users or change settings." : 'Other hosts are hidden, and their role applies on these hosts only.')}
      {value.scoped && (
        <div className="max-h-56 space-y-1 overflow-y-auto rounded-[3px] p-1.5 ring-1 ring-inset ring-line">
          {hosts.length === 0 && <p className="p-2 text-xs text-ink-100/50">No hosts yet.</p>}
          {hosts.map((h) => (
            <label key={h.id} className="flex cursor-pointer items-center gap-2.5 rounded-[2px] px-2 py-1.5 text-sm hover:bg-white/[0.04]">
              <input type="checkbox" checked={value.hosts.includes(h.id)} onChange={() => onChange({ ...value, hosts: toggleHost(value.hosts, h.id) })} className="accent-lime-400" />
              <Server className="size-3.5 shrink-0 text-ink-100/40" />
              <span className="min-w-0 flex-1 truncate text-white">{h.name}</span>
              {h.hubHost && <Badge tone="accent">hub</Badge>}
              <StatusDot tone={h.online ? 'good' : 'idle'} />
            </label>
          ))}
        </div>
      )}
      {problem && <p className="text-xs text-amber-200">{problem}</p>}
    </fieldset>
  )
}
