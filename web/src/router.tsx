import { createRootRoute, createRoute, createRouter, lazyRouteComponent, Navigate, Outlet, useRouterState } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { api, type HubState } from './api'
import { AppLayout } from './pages/AppLayout'
import { LoginPage } from './pages/Login'
import { OverviewPage } from './pages/Overview'
import { ContainersPage } from './pages/Containers'
import { HostsPage } from './pages/Hosts'
import { StacksPage } from './pages/Stacks'

// Pages used now and then (and admin-only ones) load on first visit.
const SetupPage = lazyRouteComponent(() => import('./pages/Setup'), 'SetupPage')
const InvitePage = lazyRouteComponent(() => import('./pages/Invite'), 'InvitePage')
const ResourcesPage = lazyRouteComponent(() => import('./pages/Resources'), 'ResourcesPage')
const AccountPage = lazyRouteComponent(() => import('./pages/Account'), 'AccountPage')
const AuditPage = lazyRouteComponent(() => import('./pages/Audit'), 'AuditPage')
const UsersPage = lazyRouteComponent(() => import('./pages/Users'), 'UsersPage')
const SettingsPage = lazyRouteComponent(() => import('./pages/Settings'), 'SettingsPage')
const NewStackPage = lazyRouteComponent(() => import('./pages/StackEditor'), 'NewStackPage')
const StackPage = lazyRouteComponent(() => import('./pages/StackEditor'), 'StackPage')
const NewContainerPage = lazyRouteComponent(() => import('./pages/NewContainer'), 'NewContainerPage')

function Root() {
  const path = useRouterState({ select: (s) => s.location.pathname })
  const state = useQuery({ queryKey: ['state'], queryFn: () => api<HubState>('/api/state') })
  if (state.isPending) return <Splash />
  if (state.isError) return <Splash text="Can't reach the TainerHub API." />
  if (state.data.setupRequired && path !== '/setup') return <Navigate to="/setup" />
  if (!state.data.setupRequired && path === '/setup') return <Navigate to="/login" />
  return <Outlet />
}

function Splash({ text = 'Loading…' }: { text?: string }) {
  return <div className="flex min-h-screen items-center justify-center text-sm text-ink-100/60">{text}</div>
}

const rootRoute = createRootRoute({ component: Root })
const setupRoute = createRoute({ getParentRoute: () => rootRoute, path: '/setup', component: SetupPage })
const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/login',
  component: LoginPage,
  // Where to go after signing in; LoginPage only follows same-site paths (safeNext).
  validateSearch: (s: Record<string, unknown>): { next?: string } => ({ next: typeof s.next === 'string' ? s.next : undefined }),
})
const appRoute = createRoute({ getParentRoute: () => rootRoute, id: 'app', component: AppLayout })
const overviewRoute = createRoute({ getParentRoute: () => appRoute, path: '/', component: OverviewPage })
const containersRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/containers',
  component: ContainersPage,
  // ?host=<id> shows one host; ?view=running|stopped|issues|updates picks the filter
  validateSearch: (s: Record<string, unknown>): { host?: string; view?: string } => ({
    host: typeof s.host === 'string' ? s.host : undefined,
    view: typeof s.view === 'string' ? s.view : undefined,
  }),
})
const resourcesRoute = createRoute({ getParentRoute: () => appRoute, path: '/resources', component: ResourcesPage })
const newContainerRoute = createRoute({ getParentRoute: () => appRoute, path: '/containers/new', component: NewContainerPage })
const stacksRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/stacks',
  component: StacksPage,
  // ?q= prefills the filter (links from a container's stack label)
  validateSearch: (s: Record<string, unknown>): { q?: string } => ({ q: typeof s.q === 'string' ? s.q : undefined }),
})
const newStackRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/stacks/new',
  component: NewStackPage,
  // ?host=&name= prefill the form when taking over a stack started elsewhere
  validateSearch: (s: Record<string, unknown>): { host?: string; name?: string } => ({
    host: typeof s.host === 'string' ? s.host : undefined,
    name: typeof s.name === 'string' ? s.name : undefined,
  }),
})
const stackRoute = createRoute({ getParentRoute: () => appRoute, path: '/stacks/$stackId', component: StackPage })
const hostsRoute = createRoute({ getParentRoute: () => appRoute, path: '/hosts', component: HostsPage })
const accountRoute = createRoute({ getParentRoute: () => appRoute, path: '/account', component: AccountPage })
const auditRoute = createRoute({ getParentRoute: () => appRoute, path: '/audit', component: AuditPage })
const usersRoute = createRoute({ getParentRoute: () => appRoute, path: '/users', component: UsersPage })
const settingsRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings', component: SettingsPage })
const inviteRoute = createRoute({ getParentRoute: () => rootRoute, path: '/invite', component: InvitePage })

const routeTree = rootRoute.addChildren([
  setupRoute,
  loginRoute,
  inviteRoute,
  appRoute.addChildren([overviewRoute, containersRoute, newContainerRoute, stacksRoute, newStackRoute, stackRoute, resourcesRoute, hostsRoute, accountRoute, auditRoute, usersRoute, settingsRoute]),
])

export const router = createRouter({
  routeTree,
  defaultPendingComponent: () => <p className="text-sm text-ink-100/60">Loading…</p>,
  defaultPreload: 'intent',
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
