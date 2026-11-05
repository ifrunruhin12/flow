import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { createMember, deleteUser, errorMessage, listUsers, reactivateUser, resetPassword, retryProvisioning, suspendUser } from '../../lib/api'
import type { AdminUser } from '../../lib/types'

export function MembersPanel() {
  const users = useQuery({ queryKey: ['admin', 'users'], queryFn: listUsers })
  return (
    <section className="grid gap-6 xl:grid-cols-[minmax(18rem,0.75fr)_minmax(0,1.6fr)]">
      <CreateMemberForm />
      <div>
        <h2 className="text-2xl font-black">Accounts</h2>
        <p className="mt-1 text-sm text-[var(--flow-muted)]">Provisioning state remains visible when remote work needs a retry.</p>
        {users.isPending && <p className="mt-5 text-[var(--flow-muted)]">Loading accounts…</p>}
        {users.isError && <p className="mt-5 text-[var(--flow-danger)]">{errorMessage(users.error)}</p>}
        <div className="mt-5 grid gap-4">
          {users.data?.map((user) => <MemberCard key={user.id} user={user} />)}
        </div>
      </div>
    </section>
  )
}

function CreateMemberForm() {
  const queryClient = useQueryClient()
  const [form, setForm] = useState({ username: '', displayName: '', email: '', temporaryPassword: '' })
  const mutation = useMutation({
    mutationFn: () => createMember(form),
    onSuccess: () => {
      setForm({ username: '', displayName: '', email: '', temporaryPassword: '' })
    },
		onSettled: () => queryClient.invalidateQueries({ queryKey: ['admin', 'users'] }),
  })
  const submit = (event: FormEvent) => {
    event.preventDefault()
    mutation.mutate()
  }
  return (
    <form className="panel h-fit space-y-4 p-6" onSubmit={submit}>
      <div><h2 className="text-xl font-black">Create member</h2><p className="mt-1 text-sm text-[var(--flow-muted)]">Direct creation uses the same durable provisioning path.</p></div>
      <input className="field" required placeholder="Display name" value={form.displayName} onChange={(event) => setForm({ ...form, displayName: event.target.value })} />
      <input className="field" required minLength={3} maxLength={32} placeholder="Username" value={form.username} onChange={(event) => setForm({ ...form, username: event.target.value })} />
      <input className="field" type="email" placeholder="Email (optional)" value={form.email} onChange={(event) => setForm({ ...form, email: event.target.value })} />
      <input className="field" required type="password" minLength={12} maxLength={128} placeholder="Temporary password" value={form.temporaryPassword} onChange={(event) => setForm({ ...form, temporaryPassword: event.target.value })} />
      {mutation.isError && <p className="text-sm text-[var(--flow-danger)]">{errorMessage(mutation.error)}</p>}
      <button className="button-primary w-full" disabled={mutation.isPending}>Create and provision</button>
    </form>
  )
}

function MemberCard({ user }: { user: AdminUser }) {
  const queryClient = useQueryClient()
  const [temporaryPassword, setTemporaryPassword] = useState('')
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['admin', 'users'] })
  const suspend = useMutation({ mutationFn: () => suspendUser(user.id), onSuccess: refresh })
  const reactivate = useMutation({ mutationFn: () => reactivateUser(user.id), onSuccess: refresh })
  const retry = useMutation({ mutationFn: () => retryProvisioning(user.id), onSuccess: refresh })
  const reset = useMutation({ mutationFn: () => resetPassword(user.id, temporaryPassword), onSuccess: () => { setTemporaryPassword(''); return refresh() } })
  const remove = useMutation({ mutationFn: (hard: boolean) => deleteUser(user.id, hard), onSuccess: refresh })
  const error = suspend.error ?? reactivate.error ?? retry.error ?? reset.error ?? remove.error
  return (
    <article className="panel p-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="flex flex-wrap items-center gap-2"><h3 className="text-lg font-black">{user.displayName}</h3><Badge value={user.role} /><Badge value={user.status} /><Badge value={user.provisioningState} />{user.navidromeCleanupState !== 'not_requested' && <Badge value={`cleanup: ${user.navidromeCleanupState}`} />}</div>
          <p className="mt-1 text-sm text-[var(--flow-muted)]">@{user.username}{user.email === undefined ? '' : ` · ${user.email}`}</p>
          <p className="mt-1 text-xs text-[var(--flow-muted)]">Last active: {user.lastActiveAt === undefined ? 'never' : new Date(user.lastActiveAt).toLocaleString()}</p>
        </div>
        {user.role === 'member' && (
          <div className="flex flex-wrap gap-2">
            {user.provisioningState !== 'complete' && <button className="button-secondary" onClick={() => retry.mutate()}>Resume provisioning</button>}
            {user.status === 'active' ? <button className="button-secondary" onClick={() => suspend.mutate()}>Suspend</button> : user.status === 'suspended' ? <button className="button-secondary" onClick={() => reactivate.mutate()}>Reactivate</button> : null}
          </div>
        )}
      </div>
      {user.role === 'member' && user.status !== 'deleted' && (
        <div className="mt-4 flex flex-wrap gap-2 border-t border-[var(--flow-border)] pt-4">
          <input className="field max-w-sm" type="password" minLength={12} maxLength={128} placeholder="New temporary password" value={temporaryPassword} onChange={(event) => setTemporaryPassword(event.target.value)} />
          <button className="button-secondary" disabled={temporaryPassword.length < 12 || reset.isPending} onClick={() => reset.mutate()}>Reset password</button>
          <button className="button-danger" onClick={() => { if (window.confirm(`Soft-delete @${user.username}?`)) remove.mutate(false) }}>Delete</button>
          <button className="button-danger" onClick={() => { if (window.confirm(`Delete @${user.username} and attempt Navidrome cleanup? Music files are not affected.`)) remove.mutate(true) }}>Delete + remote cleanup</button>
        </div>
      )}
      {user.role === 'member' && user.status === 'deleted' && user.navidromeCleanupState !== 'complete' && (
        <div className="mt-4 border-t border-[var(--flow-border)] pt-4">
          <button className="button-danger" onClick={() => remove.mutate(true)}>Retry Navidrome cleanup</button>
        </div>
      )}
      {error !== null && <p className="mt-3 text-sm text-[var(--flow-danger)]">{errorMessage(error)}</p>}
    </article>
  )
}

function Badge({ value }: { value: string }) {
  return <span className="rounded-full border border-[var(--flow-border)] bg-[var(--flow-surface-raised)] px-2 py-1 text-[0.68rem] font-bold uppercase tracking-wider text-[var(--flow-muted)]">{value.replaceAll('_', ' ')}</span>
}
