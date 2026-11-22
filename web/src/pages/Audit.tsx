import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Search } from 'lucide-react'
import { api, errorText, timeAgo, type AuditEntry } from '../api'
import { Badge, Card, ErrorNote, PageHeader, type Tone } from '../ui'

function toneFor(e: AuditEntry): Tone {
  if (e.action.endsWith('failed') || e.detail?.error) return 'bad'
  if (e.action.startsWith('login')) return 'info'
  if (/remove|kill|removed/.test(e.action)) return 'warn'
  if (e.action.startsWith('container.')) return 'good'
  if (e.action.startsWith('host.') || e.action.startsWith('passkey') || e.action.startsWith('recovery') || e.action.startsWith('setup')) return 'accent'
  return 'idle'
}

export function AuditPage() {
  const [q, setQ] = useState('')
  const audit = useQuery({ queryKey: ['audit'], queryFn: () => api<AuditEntry[]>('/api/audit?limit=500'), refetchInterval: 15000 })
  const rows = useMemo(() => {
    const f = q.toLowerCase()
    return (audit.data ?? []).filter((e) => !f || [e.actor, e.action, e.target, e.ip].some((s) => s.toLowerCase().includes(f)))
  }, [audit.data, q])

  return (
    <div>
      <PageHeader title="Audit log" subtitle="Every sign-in and change, newest first">
        <div className="relative">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-indigo-100/50" />
          <input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Filter"
            className="w-56 rounded-lg bg-white/[0.06] py-2 pl-9 pr-3 text-sm ring-1 ring-inset ring-white/10 outline-none focus:ring-2 focus:ring-sky-500/70"
          />
        </div>
      </PageHeader>
      <ErrorNote error={audit.isError ? errorText(audit.error) : null} />
      <Card className="overflow-hidden">
        <table className="w-full text-sm">
          <thead className="border-b border-white/[0.06] text-left text-xs font-medium uppercase tracking-wider text-indigo-100/50">
            <tr>
              <th className="px-5 py-3">When</th>
              <th className="px-5 py-3">Who</th>
              <th className="px-5 py-3">Action</th>
              <th className="px-5 py-3">Target</th>
              <th className="hidden px-5 py-3 md:table-cell">IP</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-white/[0.04]">
            {rows.map((e) => (
              <tr key={e.id} className="transition hover:bg-white/[0.02]">
                <td className="whitespace-nowrap px-5 py-3 text-indigo-100/60" title={new Date(e.at).toLocaleString()}>
                  {timeAgo(e.at)}
                </td>
                <td className="px-5 py-3 text-indigo-50">{e.actor}</td>
                <td className="px-5 py-3">
                  <Badge tone={toneFor(e)} className="mono">
                    {e.action}
                  </Badge>
                </td>
                <td className="px-5 py-3 text-indigo-100/80">
                  {e.target}
                  {e.detail?.error && <div className="text-xs text-rose-300">{e.detail.error}</div>}
                </td>
                <td className="mono hidden px-5 py-3 text-xs text-indigo-100/50 md:table-cell">{e.ip}</td>
              </tr>
            ))}
            {rows.length === 0 && (
              <tr>
                <td colSpan={5} className="px-5 py-10 text-center text-indigo-100/50">
                  {audit.isPending ? 'Loading…' : 'No entries.'}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </Card>
    </div>
  )
}
