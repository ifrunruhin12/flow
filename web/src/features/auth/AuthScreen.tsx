import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { errorMessage, login, registerRequest } from '../../lib/api'

export function AuthScreen({ notice, onNotice }: { notice: string; onNotice: (value: string) => void }) {
  const [view, setView] = useState<'login' | 'register'>('login')
  return (
    <main className="grid min-h-screen place-items-center px-5 py-10">
      <div className="w-full max-w-md">
        <div className="mb-7 text-center">
          <p className="text-xs font-bold uppercase tracking-[0.3em] text-[var(--flow-accent-strong)]">Private music, thoughtfully shared</p>
          <h1 className="mt-3 text-5xl font-black tracking-tight">Flow</h1>
        </div>
        <section className="panel p-7">
          <div className="mb-6 grid grid-cols-2 rounded-xl bg-[var(--flow-surface-raised)] p-1">
            {(['login', 'register'] as const).map((item) => (
              <button
                key={item}
                className={`rounded-lg px-3 py-2 text-sm font-bold ${view === item ? 'bg-[var(--flow-accent)]' : 'text-[var(--flow-muted)]'}`}
                onClick={() => {
                  setView(item)
                  onNotice('')
                }}
              >
                {item === 'login' ? 'Sign in' : 'Request access'}
              </button>
            ))}
          </div>
          {notice !== '' && <p className="mb-4 rounded-xl border border-[var(--flow-success)] p-3 text-sm text-[var(--flow-success)]">{notice}</p>}
          {view === 'login' ? <LoginForm /> : <RegisterForm onComplete={onNotice} onLogin={() => setView('login')} />}
        </section>
      </div>
    </main>
  )
}

function LoginForm() {
  const queryClient = useQueryClient()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const mutation = useMutation({
    mutationFn: () => login(username, password),
    onSuccess: (user) => queryClient.setQueryData(['session'], user),
  })
  const submit = (event: FormEvent) => {
    event.preventDefault()
    mutation.mutate()
  }
  return (
    <form className="space-y-4" onSubmit={submit}>
      <label className="block text-sm font-bold">
        Username
        <input className="field mt-2" autoComplete="username" required value={username} onChange={(event) => setUsername(event.target.value)} />
      </label>
      <label className="block text-sm font-bold">
        Password
        <input className="field mt-2" type="password" autoComplete="current-password" required value={password} onChange={(event) => setPassword(event.target.value)} />
      </label>
      {mutation.isError && <p className="text-sm text-[var(--flow-danger)]">{errorMessage(mutation.error)}</p>}
      <button className="button-primary w-full" disabled={mutation.isPending}>Sign in</button>
    </form>
  )
}

function RegisterForm({ onComplete, onLogin }: { onComplete: (value: string) => void; onLogin: () => void }) {
  const [displayName, setDisplayName] = useState('')
  const [email, setEmail] = useState('')
  const [desiredUsername, setDesiredUsername] = useState('')
  const mutation = useMutation({
    mutationFn: () => registerRequest(displayName, email, desiredUsername),
    onSuccess: (requestId) => {
      onComplete(`Request #${requestId} received. The administrator will contact you out-of-band.`)
      onLogin()
    },
  })
  return (
    <form className="space-y-4" onSubmit={(event) => { event.preventDefault(); mutation.mutate() }}>
      <label className="block text-sm font-bold">Display name<input className="field mt-2" required value={displayName} onChange={(event) => setDisplayName(event.target.value)} /></label>
      <label className="block text-sm font-bold">Email<input className="field mt-2" type="email" autoComplete="email" required value={email} onChange={(event) => setEmail(event.target.value)} /></label>
      <label className="block text-sm font-bold">Desired username<input className="field mt-2" required minLength={3} maxLength={32} value={desiredUsername} onChange={(event) => setDesiredUsername(event.target.value)} /></label>
      {mutation.isError && <p className="text-sm text-[var(--flow-danger)]">{errorMessage(mutation.error)}</p>}
      <button className="button-primary w-full" disabled={mutation.isPending}>Send request</button>
    </form>
  )
}
