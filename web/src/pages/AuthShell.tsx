import type { ReactNode } from 'react'
import { LogoMark } from './Logo'

/** Full-screen backdrop for sign-in and setup. */
export function AuthShell({ title, subtitle, children }: { title: string; subtitle?: string; children: ReactNode }) {
  return (
    <div className="relative flex min-h-screen items-center justify-center overflow-hidden p-4">
      <div className="grid-bg pointer-events-none absolute inset-0" />
      <div className="pointer-events-none absolute left-1/2 top-1/3 size-[36rem] -translate-x-1/2 -translate-y-1/2 rounded-full bg-gradient-to-br from-sky-500/25 via-purple-500/20 to-fuchsia-500/10 blur-3xl" />
      <div className="relative w-full max-w-sm animate-[pop_250ms_ease-out]">
        <div className="mb-8 flex flex-col items-center text-center">
          <LogoMark className="mb-5 size-14 drop-shadow-[0_10px_30px_rgba(56,189,248,0.35)]" />
          <h1 className="font-display text-3xl font-bold tracking-tight text-white">{title}</h1>
          {subtitle && <p className="mt-2 text-sm text-indigo-100/60">{subtitle}</p>}
        </div>
        <div className="rounded-3xl bg-white/[0.06] p-6 shadow-2xl shadow-black/40 ring-1 ring-white/15 backdrop-blur-xl">{children}</div>
        <p className="mt-6 text-center text-xs text-indigo-100/40">Secured with passkeys · no passwords stored</p>
      </div>
    </div>
  )
}
