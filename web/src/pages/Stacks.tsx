import { useMemo, useState } from 'react'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { Ellipsis, Layers, Play, Plus, RefreshCw, RotateCw, Search, Square, Download, ArrowDownToLine, ExternalLink, PencilLine } from 'lucide-react'
import { can, timeAgo, type StackAction, type StackSummary } from '../api'
import { Badge, Button, Card, EmptyState, IconButton, Menu, PageHeader, StatusDot, cx, type MenuItem } from '../ui'
import { ACTION_LABELS, StackStatusBadge, stackTone, useStackAction, useStacks } from '../stacks'
import { useMe } from './AppLayout'

const actionIcons: Record<StackAction, typeof Play> = {
  deploy: RefreshCw,
  pull: Download,
  start: Play,
  stop: Square,
  restart: RotateCw,
  down: ArrowDownToLine,
}

export function StacksPage() {
  const me = useMe()
  const admin = can(me.user, 'admin')
  const operator = can(me.user, 'operator')
  const stacks = useStacks()
  const { run } = useStackAction()
  const search = useSearch({ strict: false }) as { q?: string }
  const [filter, setFilter] = useState(search.q ?? '')
  const navigate = useNavigate()

  const list = useMemo(() => {
    const f = filter.trim().toLowerCase()
    return (stacks.data ?? []).filter((s) => !f || s.name.includes(f) || s.hostName.toLowerCase().includes(f))
  }, [stacks.data, filter])
  const managed = list.filter((s) => s.managed)
  const external = list.filter((s) => !s.managed)

  return (
    <div>
      <PageHeader
        title="Stacks"
        subtitle={stacks.data ? `${stacks.data.filter((s) => s.managed).length} managed by TainerHub · ${stacks.data.filter((s) => !s.managed).length} started elsewhere` : undefined}
      >
        <div className="relative">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-ink-100/50" />
          <input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter stacks"
            aria-label="Filter stacks"
            className="w-44 rounded-[2px] bg-ink-950/70 py-2 pl-9 pr-3 text-sm text-white ring-1 ring-inset ring-white/12 outline-none placeholder:text-ink-100/35 focus:ring-lime-400/60"
          />
        </div>
        {admin && (
          <Button variant="primary" icon={Plus} onClick={() => navigate({ to: '/stacks/new', search: {} })}>
            New stack
          </Button>
        )}
      </PageHeader>

      {stacks.isPending ? (
        <p className="text-sm text-ink-100/60">Loading…</p>
      ) : stacks.data?.length === 0 ? (
        <EmptyState icon={Layers} title="No stacks yet">
          A stack is a compose file TainerHub deploys to one host and keeps track of.
          {admin ? ' Create one to deploy your first app.' : ' An admin can create one.'}
        </EmptyState>
      ) : (
        <div className="space-y-8">
          {managed.length > 0 && (
            <section>
              <h2 className="mb-3 text-xs font-semibold uppercase tracking-wider text-ink-100/50">Managed by TainerHub</h2>
              <Card className="divide-y divide-white/[0.06]">
                {managed.map((s) => (
                  <StackRow key={s.id} s={s} actions={operator ? managedActions(s, run) : []} />
                ))}
              </Card>
            </section>
          )}
          {external.length > 0 && (
            <section>
              <h2 className="mb-1 text-xs font-semibold uppercase tracking-wider text-ink-100/50">Started outside TainerHub</h2>
              <p className="mb-3 text-sm text-ink-100/60">
                Compose projects found running on your hosts. Manage their containers from the Containers page
                {admin ? ', or take one over by giving TainerHub its compose file.' : '.'}
              </p>
              <Card className="divide-y divide-white/[0.06]">
                {external.map((s) => (
                  <StackRow key={`${s.hostId}/${s.name}`} s={s} actions={[]} adopt={admin} />
                ))}
              </Card>
            </section>
          )}
          {list.length === 0 && <p className="text-sm text-ink-100/60">No stacks match “{filter}”.</p>}
        </div>
      )}
    </div>
  )
}

function managedActions(s: StackSummary, run: (s: StackSummary, a: StackAction) => void): MenuItem[] {
  const busy = !!s.job || !s.online
  const has = s.containers > 0
  const show: Record<StackAction, boolean> = {
    deploy: true,
    pull: true,
    start: has && s.running < s.containers,
    stop: s.running > 0,
    restart: s.running > 0,
    down: has,
  }
  return (Object.keys(ACTION_LABELS) as StackAction[])
    .filter((a) => show[a] && !busy)
    .map((a) => ({ label: ACTION_LABELS[a], icon: actionIcons[a], onClick: () => run(s, a), danger: a === 'down' || a === 'stop' }))
}

function StackRow({ s, actions, adopt }: { s: StackSummary; actions: MenuItem[]; adopt?: boolean }) {
  const navigate = useNavigate()
  const name = (
    <span className="flex items-center gap-2">
      <StatusDot tone={s.online ? stackTone(s) : 'idle'} pulse={s.status === 'running'} />
      <span className="truncate font-semibold text-white">{s.name}</span>
    </span>
  )
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 px-5 py-3.5">
      <div className="min-w-0 flex-1">
        {s.id ? (
          <Link to="/stacks/$stackId" params={{ stackId: s.id }} className="hover:underline">
            {name}
          </Link>
        ) : (
          name
        )}
        <div className="mt-0.5 truncate pl-[18px] text-xs text-ink-100/50">
          {s.hostName}
          {s.managed && (s.deployedAt ? ` · deployed ${timeAgo(s.deployedAt)}` : ' · never deployed')}
          {s.managed && s.updatedBy && ` · edited by ${s.updatedBy} ${timeAgo(s.updatedAt)}`}
        </div>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        {s.pending && s.deployedAt && (
          <Badge tone="warn" className="gap-1">
            <PencilLine className="size-3" />
            changes not deployed
          </Badge>
        )}
        {s.containers > 0 && (
          <span className="text-xs tabular-nums text-ink-100/60">
            {s.running}/{s.containers} running
          </span>
        )}
        <StackStatusBadge s={s} />
      </div>
      <div className={cx('flex w-9 justify-end', adopt && 'w-auto')}>
        {adopt ? (
          <Button variant="ghost" icon={ExternalLink} className="!px-3 !py-1.5 text-xs" onClick={() => navigate({ to: '/stacks/new', search: { host: s.hostId, name: s.name } })}>
            Manage in TainerHub
          </Button>
        ) : (
          actions.length > 0 && <Menu trigger={(toggle) => <IconButton icon={Ellipsis} label={`Actions for ${s.name}`} onClick={toggle} />} items={actions} />
        )}
      </div>
    </div>
  )
}
