import { useId } from 'react'

/**
 * TainerHub mark: a server rack whose bays each hold a container, with a
 * status light per bay. Lime, mint and teal containers; sun = attention.
 */
export function LogoMark({ className = 'size-8' }: { className?: string }) {
  const id = useId().replace(/:/g, '')
  const ribs = (y: number) => `M19.7 ${y}v5 M24.3 ${y}v5 M29 ${y}v5 M33.7 ${y}v5 M38.3 ${y}v5`
  return (
    <svg viewBox="0 0 64 64" className={className} aria-hidden="true">
      <defs>
        <linearGradient id={`${id}edge`} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#d4f77a" />
          <stop offset=".5" stopColor="#c4f04a" />
          <stop offset="1" stopColor="#7fa81a" />
        </linearGradient>
        <linearGradient id={`${id}srv`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor="#22302a" />
          <stop offset="1" stopColor="#15201b" />
        </linearGradient>
        <linearGradient id={`${id}m`} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#e4fbab" />
          <stop offset="1" stopColor="#c4f04a" />
        </linearGradient>
        <linearGradient id={`${id}s`} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#a7f3d0" />
          <stop offset="1" stopColor="#34d399" />
        </linearGradient>
        <linearGradient id={`${id}p`} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor="#99f6e4" />
          <stop offset="1" stopColor="#14b8a6" />
        </linearGradient>
      </defs>
      <rect width="64" height="64" rx="10" fill="#0a1210" />
      <rect x=".75" y=".75" width="62.5" height="62.5" rx="9.25" fill="none" stroke={`url(#${id}edge)`} strokeOpacity=".55" strokeWidth="1.5" />
      <rect x="10" y="9" width="44" height="46" rx="6" fill={`url(#${id}srv)`} stroke="#34443d" strokeWidth="1.2" />
      <rect x="15" y="14" width="28" height="10" rx="2" fill={`url(#${id}m)`} />
      <rect x="15" y="27" width="28" height="10" rx="2" fill={`url(#${id}s)`} />
      <rect x="15" y="40" width="28" height="10" rx="2" fill={`url(#${id}p)`} />
      <path d={`${ribs(16.5)} ${ribs(29.5)} ${ribs(42.5)}`} stroke="#0a1210" strokeOpacity=".32" strokeWidth="1.3" strokeLinecap="round" />
      <circle cx="48" cy="19" r="1.9" fill="#c4f04a" />
      <circle cx="48" cy="32" r="1.9" fill="#c4f04a" />
      <circle cx="48" cy="45" r="1.9" fill="#fbbf24" />
    </svg>
  )
}

export function Logo() {
  return (
    <span className="flex items-center gap-2.5">
      <LogoMark className="size-9" />
      <span className="font-display text-[19px] font-bold tracking-tight text-white">
        Tainer<span className="text-lime-400">Hub</span>
      </span>
    </span>
  )
}
