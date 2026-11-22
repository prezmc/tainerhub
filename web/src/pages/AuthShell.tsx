import type { ReactNode } from 'react'
import { LogoMark } from './Logo'

/** Full-screen backdrop for sign-in and setup. */
export function AuthShell({ title, subtitle, children }: { title: string; subtitle?: string; children: ReactNode }) {
  return (
    <div className="relative flex min-h-screen items-center justify-center overflow-hidden p-4">
      <div className="grid-bg pointer-events-none absolute inset-0" />
      <div className="relative w-full max-w-sm animate-[pop_250ms_ease-out]">
        <div className="mb-8 flex flex-col items-center text-center">
          <LogoMark className="mb-5 size-14 " />
          <h1 className="font-display text-3xl font-bold tracking-tight text-white">{title}</h1>
          {subtitle && <p className="mt-2 text-sm text-ink-100/60">{subtitle}</p>}
        </div>
        <div className="rounded-md border-t-2 border-lime-400 bg-panel p-6 shadow-2xl shadow-black/50 ring-1 ring-line">{children}</div>
        <p className="mt-6 text-center text-xs text-ink-100/40">Secured with passkeys · no passwords stored</p>
      </div>
    </div>
  )
}
