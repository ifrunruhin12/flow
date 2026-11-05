import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { changePassword, errorMessage } from '../../lib/api'

export function PasswordChange({ username }: { username: string }) {
  const queryClient = useQueryClient()
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const mutation = useMutation({
    mutationFn: () => changePassword(currentPassword, newPassword),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['session'] }),
  })
  const submit = (event: FormEvent) => {
    event.preventDefault()
    mutation.mutate()
  }
  return (
    <main className="grid min-h-screen place-items-center px-5">
      <section className="panel w-full max-w-md p-7">
        <p className="text-sm font-bold text-[var(--flow-warning)]">Password change required</p>
        <h1 className="mt-2 text-3xl font-black">Welcome, {username}</h1>
        <p className="mt-3 text-sm leading-6 text-[var(--flow-muted)]">Replace the temporary password before using any other part of Flow. Your current session will remain active; other sessions are revoked.</p>
        <form className="mt-6 space-y-4" onSubmit={submit}>
          <label className="block text-sm font-bold">Temporary password<input className="field mt-2" type="password" autoComplete="current-password" required value={currentPassword} onChange={(event) => setCurrentPassword(event.target.value)} /></label>
          <label className="block text-sm font-bold">New password<input className="field mt-2" type="password" autoComplete="new-password" required minLength={12} maxLength={128} value={newPassword} onChange={(event) => setNewPassword(event.target.value)} /></label>
          {mutation.isError && <p className="text-sm text-[var(--flow-danger)]">{errorMessage(mutation.error)}</p>}
          <button className="button-primary w-full" disabled={mutation.isPending}>Replace password</button>
        </form>
      </section>
    </main>
  )
}
