import { useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CircleCheck, CircleAlert, ExternalLink, Lock, LockOpen, ShieldCheck } from 'lucide-react'
import { api, errorText } from '../api'
import { Badge, Button, Card, CopyButton, ErrorNote, Input, PageHeader, useConfirm, useToast } from '../ui'

type HTTPSSettings = {
  publicURL: string
  trustedProxies: string[]
  lockedByEnv: boolean
  passkeys: boolean
  httpPort: string
  viaHTTPS: boolean
}

type TestResult = {
  ok: boolean
  proxyIp?: string
  forwardedProto?: string
  host?: string
  error?: string
  tlsError?: string
}

// Settings → HTTPS & passkeys: walks through putting TainerHub behind a
// reverse proxy (Nginx Proxy Manager on its own server), checks it from the
// hub's side, and switches it on without a restart.
export function SettingsPage() {
  const qc = useQueryClient()
  const toast = useToast()
  const confirm = useConfirm()
  const cfg = useQuery({ queryKey: ['settings-https'], queryFn: () => api<HTTPSSettings>('/api/settings/https') })
  const [domain, setDomain] = useState('')
  const [trusted, setTrusted] = useState('')
  const [result, setResult] = useState<TestResult | null>(null)
  const [error, setError] = useState<string | null>(null)

  const address = normalize(domain)
  const hubHost = window.location.hostname
  const port = cfg.data?.httpPort || '8585'

  const test = useMutation({
    mutationFn: () => api<TestResult>('/api/settings/https/test', { method: 'POST', body: { url: address } }),
    onMutate: () => {
      setError(null)
      setResult(null)
    },
    onSuccess: (r) => {
      setResult(r)
      if (r.proxyIp && !trusted) setTrusted(r.proxyIp)
    },
    onError: (e) => setError(errorText(e)),
  })
  const enable = useMutation({
    mutationFn: (force: boolean) => api('/api/settings/https', { method: 'PUT', body: { url: address, trusted, force } }),
    onSuccess: () => {
      toast('good', 'HTTPS is on')
      qc.invalidateQueries()
    },
    onError: (e) => setError(errorText(e)),
  })
  const disable = useMutation({
    mutationFn: () => api('/api/settings/https', { method: 'DELETE' }),
    onSuccess: () => {
      toast('good', 'HTTPS is off. Passkeys are paused; passwords still work.')
      setResult(null)
      qc.invalidateQueries()
    },
    onError: (e) => toast('bad', errorText(e)),
  })

  if (!cfg.data) return <PageHeader title="Settings" />
  const s = cfg.data
  const on = !!s.publicURL

  return (
    <div className="max-w-4xl space-y-6">
      <PageHeader title="Settings" subtitle="HTTPS and passkeys, through the reverse proxy you already run." />

      <Card className="flex flex-wrap items-center gap-4 px-5 py-4">
        {on ? <Lock className="size-5 text-lime-300" /> : <LockOpen className="size-5 text-ink-100/50" />}
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-semibold text-white">{on ? 'HTTPS is on' : 'HTTPS is off'}</span>
            <Badge tone={s.passkeys ? 'good' : 'idle'}>{s.passkeys ? 'passkeys on' : 'passkeys off'}</Badge>
            {s.lockedByEnv && <Badge tone="info">set in compose file</Badge>}
          </div>
          <p className="text-sm text-ink-100/60">
            {on ? (
              <>
                Secure address{' '}
                <a href={s.publicURL} className="text-lime-300 underline-offset-2 hover:underline">
                  {s.publicURL}
                </a>
                {s.trustedProxies.length > 0 && <> · proxy {s.trustedProxies.join(', ')}</>}. The LAN address keeps working with passwords.
              </>
            ) : (
              <>You're using plain HTTP with passwords, which is fine on a home network. HTTPS adds passkeys and is needed for access from outside.</>
            )}
          </p>
        </div>
        {on && !s.viaHTTPS && (
          <a href={s.publicURL}>
            <Button variant="primary" icon={ExternalLink}>
              Open secure address
            </Button>
          </a>
        )}
        {on && !s.lockedByEnv && (
          <Button
            variant="dangerGhost"
            loading={disable.isPending}
            onClick={async () => {
              if (
                await confirm({
                  title: 'Turn HTTPS off?',
                  body: 'Passkeys stop working until you turn it back on (they are kept). Everyone can still sign in with a password at the LAN address. Your proxy keeps forwarding; remove its proxy host if you no longer want it.',
                  confirm: 'Turn off',
                  danger: true,
                })
              )
                disable.mutate()
            }}
          >
            Turn off
          </Button>
        )}
      </Card>

      {s.lockedByEnv ? (
        <Card className="px-5 py-4 text-sm text-ink-100/70">
          The secure address is set with <code className="text-white">TAINERHUB_PUBLIC_URL</code> in the hub's compose file, so it's managed there. Remove that line
          and run <code className="text-white">docker compose up -d</code> to manage it here instead.
        </Card>
      ) : (
        !on && (
          <>
            <Step n={1} title="Pick an address and point it at your proxy">
              <p>
                Choose a name like <code className="text-white">tainerhub.yourdomain.com</code>. In your DNS (your domain provider, router or Pi-hole), point it at
                the <b className="text-white">Nginx Proxy Manager server's</b> IP, the same way as your other proxied apps.
              </p>
              <div className="max-w-md pt-1">
                <Input label="Secure address" placeholder="tainerhub.example.com" value={domain} onChange={(e) => setDomain(e.target.value)} />
              </div>
            </Step>

            <Step n={2} title="Add a Proxy Host in Nginx Proxy Manager">
              <p>
                In NPM: <b className="text-white">Hosts → Proxy Hosts → Add Proxy Host</b>, then type exactly these values.
              </p>
              <Values
                title="Details tab"
                rows={[
                  ['Domain Names', address ? new URL(address).host : 'your address from step 1', !!address],
                  ['Scheme', 'http', true],
                  ['Forward Hostname / IP', hubHost, true],
                  ['Forward Port', port, true],
                  ['Cache Assets', 'off', false],
                  ['Block Common Exploits', 'on', false],
                  ['Websockets Support', 'on (needed for live updates and terminals)', false],
                ]}
              />
              <Values
                title="SSL tab"
                rows={[
                  ['SSL Certificate', "Request a new SSL Certificate (Let's Encrypt), or pick your wildcard certificate", false],
                  ['Force SSL', 'on', false],
                  ['HTTP/2 Support', 'on', false],
                ]}
              />
              {isLocalName(hubHost) && (
                <p className="text-amber-200">
                  You opened TainerHub as <code>{hubHost}</code>. For the forward host, use this server's LAN IP instead (the address NPM can reach).
                </p>
              )}
              <p className="text-ink-100/50">
                Only port {port} goes through the proxy. Agents connect to the hub on port 7443 directly; leave that out of NPM.
              </p>
            </Step>

            <Step n={3} title="Test it from the hub">
              <p>The hub requests your secure address and checks the answer comes back to itself through the proxy.</p>
              <div className="flex flex-wrap items-center gap-2">
                <Button variant="primary" icon={ShieldCheck} disabled={!address} loading={test.isPending} onClick={() => test.mutate()}>
                  Test {address ? new URL(address).host : 'address'}
                </Button>
              </div>
              {result && <TestReport r={result} port={port} />}
            </Step>

            {result && (result.ok || result.tlsError || result.proxyIp) && (
              <Step n={4} title="Turn on HTTPS">
                <p>
                  TainerHub trusts the proxy's IP to say visitors arrived over HTTPS. The test found it; change it only if your proxy uses more than one address.
                </p>
                <div className="max-w-md">
                  <Input label="Proxy IP (trusted)" value={trusted} onChange={(e) => setTrusted(e.target.value)} hint="IP addresses or ranges, separated by commas" />
                </div>
                <div className="flex flex-wrap items-center gap-2">
                  {result.ok ? (
                    <Button variant="primary" icon={Lock} loading={enable.isPending} onClick={() => enable.mutate(false)}>
                      Turn on HTTPS
                    </Button>
                  ) : (
                    <Button
                      variant="secondary"
                      loading={enable.isPending}
                      onClick={async () => {
                        if (
                          await confirm({
                            title: 'Turn on without a passing test?',
                            body: "The hub couldn't confirm everything works. Browsers may refuse passkeys on this address until the certificate is fixed. You can turn it off again here.",
                            confirm: 'Turn on anyway',
                          })
                        )
                          enable.mutate(true)
                      }}
                    >
                      Turn on anyway
                    </Button>
                  )}
                  <span className="text-sm text-ink-100/50">Applies straight away, no restart. Then open the secure address and add a passkey under Account.</span>
                </div>
              </Step>
            )}
            <ErrorNote error={error} />
          </>
        )
      )}
    </div>
  )
}

function Step({ n, title, children }: { n: number; title: string; children: ReactNode }) {
  return (
    <Card className="px-5 py-4">
      <div className="mb-2 flex items-center gap-3">
        <span className="grid size-6 place-items-center rounded-full bg-lime-400/15 font-display text-xs font-bold text-lime-300">{n}</span>
        <h2 className="font-display text-lg font-semibold text-white">{title}</h2>
      </div>
      <div className="space-y-3 pl-9 text-sm text-ink-100/70">{children}</div>
    </Card>
  )
}

function Values({ title, rows }: { title: string; rows: [string, string, boolean][] }) {
  return (
    <div>
      <div className="mb-1 text-xs font-semibold uppercase tracking-wider text-ink-100/50">{title}</div>
      <div className="divide-y divide-line rounded-[3px] ring-1 ring-line">
        {rows.map(([k, v, copy]) => (
          <div key={k} className="flex items-center gap-3 px-3 py-1.5">
            <span className="w-48 shrink-0 text-ink-100/60">{k}</span>
            <span className="min-w-0 flex-1 font-mono text-[13px] text-white">{v}</span>
            {copy && <CopyButton text={v} />}
          </div>
        ))}
      </div>
    </div>
  )
}

function TestReport({ r, port }: { r: TestResult; port: string }) {
  const lines: [boolean, ReactNode][] = []
  if (r.error) lines.push([false, r.error])
  if (r.tlsError) lines.push([false, <>The certificate isn't trusted ({r.tlsError}). In NPM's SSL tab, request a Let's Encrypt certificate for this name.</>])
  if (!r.error) {
    lines.push([true, <>The address reaches this TainerHub through the proxy{r.proxyIp && <> (proxy IP {r.proxyIp})</>}.</>])
    if (!r.tlsError) lines.push([true, 'The certificate is valid.'])
    if (r.forwardedProto === 'https') lines.push([true, 'The proxy tells TainerHub the visitor used HTTPS.'])
    else lines.push([false, <>The proxy didn't send X-Forwarded-Proto: https. NPM does this on its own; another proxy needs it set (forwarding to port {port}).</>])
  }
  return (
    <ul className="space-y-1.5">
      {lines.map(([ok, text], i) => (
        <li key={i} className={ok ? 'flex gap-2 text-lime-200' : 'flex gap-2 text-amber-200'}>
          {ok ? <CircleCheck className="mt-0.5 size-4 shrink-0" /> : <CircleAlert className="mt-0.5 size-4 shrink-0" />}
          <span>{text}</span>
        </li>
      ))}
    </ul>
  )
}

function normalize(d: string): string {
  let v = d.trim().replace(/\/+$/, '')
  if (!v) return ''
  if (!/^[a-z]+:\/\//i.test(v)) v = 'https://' + v
  try {
    const u = new URL(v)
    return u.protocol === 'https:' && u.hostname.includes('.') ? u.origin : ''
  } catch {
    return ''
  }
}

const isLocalName = (h: string) => h === 'localhost' || h === '127.0.0.1' || h === '::1' || h.endsWith('.localhost')
