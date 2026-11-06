import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { AdminDashboard } from '../features/admin'
import { AuthScreen, PasswordChange } from '../features/auth'
import { LibraryExperience } from '../features/library'
import { PlayerBar, PlayerProvider } from '../features/player'
import { errorMessage, getSession, logout } from '../lib/api'

export function App() {
  const queryClient = useQueryClient()
  const [notice, setNotice] = useState('')
  const [area, setArea] = useState<'library' | 'administration'>('library')
  const session = useQuery({ queryKey: ['session'], queryFn: getSession, retry: false })
  const logoutMutation = useMutation({
    mutationFn: logout,
    onSuccess: () => {
      queryClient.setQueryData(['session'], null)
      queryClient.removeQueries({ queryKey: ['admin'] })
      queryClient.removeQueries({ queryKey: ['library'] })
    },
  })

  if (session.isPending) {
    return <CenteredMessage title="Opening Flow" detail="Checking your session…" />
  }
  if (session.isError) {
    return <CenteredMessage title="Flow is unavailable" detail={errorMessage(session.error)} />
  }
  if (session.data === null) {
    return <AuthScreen notice={notice} onNotice={setNotice} />
  }
  if (session.data.mustChangePassword) {
    return <PasswordChange username={session.data.username} />
  }

  return (
    <PlayerProvider key={session.data.id}>
    <div className="mx-auto min-h-screen max-w-7xl px-5 py-6 pb-52 sm:px-8">
      <header className="mb-8 flex flex-wrap items-center justify-between gap-4">
        <div>
          <p className="text-xs font-bold uppercase tracking-[0.28em] text-[var(--flow-accent-strong)]">Private listening</p>
          <h1 className="mt-1 text-3xl font-black tracking-tight">Flow</h1>
        </div>
        <div className="flex items-center gap-3">
          <div className="text-right text-sm">
            <p className="font-bold">{session.data.displayName}</p>
            <p className="text-[var(--flow-muted)]">@{session.data.username}</p>
          </div>
          <button className="button-secondary" disabled={logoutMutation.isPending} onClick={() => logoutMutation.mutate()}>
            Sign out
          </button>
        </div>
      </header>
      {session.data.role === 'admin' && (
        <nav className="mb-7 flex gap-2" aria-label="Flow areas">
          <button className={area === 'library' ? 'button-primary' : 'button-secondary'} onClick={() => setArea('library')}>Music</button>
          <button className={area === 'administration' ? 'button-primary' : 'button-secondary'} onClick={() => setArea('administration')}>Administration</button>
        </nav>
      )}
      {session.data.role === 'admin' && area === 'administration' ? (
        <AdminDashboard />
      ) : (
        <LibraryExperience
          isAdmin={session.data.role === 'admin'}
        />
      )}
      <PlayerBar />
    </div>
    </PlayerProvider>
  )
}

function CenteredMessage({ title, detail }: { title: string; detail: string }) {
  return (
    <main className="grid min-h-screen place-items-center px-5">
      <section className="panel w-full max-w-lg p-8 text-center">
        <h1 className="text-2xl font-black">{title}</h1>
        <p className="mt-3 text-[var(--flow-muted)]">{detail}</p>
      </section>
    </main>
  )
}
