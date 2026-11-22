import { createRootRoute, createRoute, createRouter, Navigate, Outlet, useRouterState } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { api } from './api'
import { AppLayout } from './pages/AppLayout'
import { SetupPage } from './pages/Setup'
import { LoginPage } from './pages/Login'
import { OverviewPage } from './pages/Overview'
import { ContainersPage } from './pages/Containers'
import { ResourcesPage } from './pages/Resources'
import { HostsPage } from './pages/Hosts'
import { AccountPage } from './pages/Account'
import { AuditPage } from './pages/Audit'
import { StacksPage } from './pages/Stacks'
import { NewStackPage, StackPage } from './pages/StackEditor'

function Root() {
  const path = useRouterState({ select: (s) => s.location.pathname })
  const state = useQuery({ queryKey: ['state'], queryFn: () => api<{ setupRequired: boolean; version: string }>('/api/state') })
  if (state.isPending) return <Splash />
  if (state.isError) return <Splash text="Can't reach the TainerHub API." />
  if (state.data.setupRequired && path !== '/setup') return <Navigate to="/setup" />
  if (!state.data.setupRequired && path === '/setup') return <Navigate to="/login" />
  return <Outlet />
}

function Splash({ text = 'Loading…' }: { text?: string }) {
  return <div className="flex min-h-screen items-center justify-center text-sm text-indigo-100/60">{text}</div>
}

const rootRoute = createRootRoute({ component: Root })
const setupRoute = createRoute({ getParentRoute: () => rootRoute, path: '/setup', component: SetupPage })
const loginRoute = createRoute({ getParentRoute: () => rootRoute, path: '/login', component: LoginPage })
const appRoute = createRoute({ getParentRoute: () => rootRoute, id: 'app', component: AppLayout })
const overviewRoute = createRoute({ getParentRoute: () => appRoute, path: '/', component: OverviewPage })
const containersRoute = createRoute({ getParentRoute: () => appRoute, path: '/containers', component: ContainersPage })
const resourcesRoute = createRoute({ getParentRoute: () => appRoute, path: '/resources', component: ResourcesPage })
const stacksRoute = createRoute({ getParentRoute: () => appRoute, path: '/stacks', component: StacksPage })
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

const routeTree = rootRoute.addChildren([
  setupRoute,
  loginRoute,
  appRoute.addChildren([overviewRoute, containersRoute, stacksRoute, newStackRoute, stackRoute, resourcesRoute, hostsRoute, accountRoute, auditRoute]),
])

export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
