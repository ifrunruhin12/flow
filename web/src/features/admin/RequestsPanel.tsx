import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { approveRequest, errorMessage, listRequests, rejectRequest } from '../../lib/api'
import type { MembershipRequest } from '../../lib/types'

export function RequestsPanel() {
  const requests = useQuery({ queryKey: ['admin', 'requests'], queryFn: listRequests })
  return (
    <section>
      <div className="mb-5">
        <h2 className="text-2xl font-black">Pending access requests</h2>
        <p className="mt-1 text-sm text-[var(--flow-muted)]">Approval creates and provisions the account immediately. Share the temporary Flow password out-of-band.</p>
      </div>
      {requests.isPending && <p className="text-[var(--flow-muted)]">Loading requests…</p>}
      {requests.isError && <p className="text-[var(--flow-danger)]">{errorMessage(requests.error)}</p>}
      <div className="grid gap-4">
        {requests.data?.map((request) => <RequestCard key={request.id} request={request} />)}
        {requests.data?.length === 0 && <div className="panel p-7 text-[var(--flow-muted)]">No pending requests.</div>}
      </div>
    </section>
  )
}

function RequestCard({ request }: { request: MembershipRequest }) {
  const queryClient = useQueryClient()
  const [temporaryPassword, setTemporaryPassword] = useState('')
  const [note, setNote] = useState('')
  const approve = useMutation({
    mutationFn: () => approveRequest(request.id, temporaryPassword, note),
    onSettled: () => Promise.all([
      queryClient.invalidateQueries({ queryKey: ['admin', 'requests'] }),
      queryClient.invalidateQueries({ queryKey: ['admin', 'users'] }),
    ]),
  })
  const reject = useMutation({
    mutationFn: () => rejectRequest(request.id, note),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['admin', 'requests'] }),
  })
  const busy = approve.isPending || reject.isPending
  return (
    <article className="panel grid gap-5 p-6 lg:grid-cols-[1fr_1.2fr]">
      <div>
        <p className="text-xs font-bold uppercase tracking-wider text-[var(--flow-accent-strong)]">Request #{request.id}</p>
        <h3 className="mt-2 text-xl font-black">{request.displayName}</h3>
        <p className="mt-2 text-sm text-[var(--flow-muted)]">@{request.desiredUsername}</p>
        <p className="text-sm text-[var(--flow-muted)]">{request.email}</p>
      </div>
      <div className="space-y-3">
        <input className="field" type="password" minLength={12} maxLength={128} placeholder="Temporary password (12–128 characters)" value={temporaryPassword} onChange={(event) => setTemporaryPassword(event.target.value)} />
        <textarea className="field min-h-20" placeholder="Internal note (optional)" value={note} onChange={(event) => setNote(event.target.value)} />
        {(approve.isError || reject.isError) && <p className="text-sm text-[var(--flow-danger)]">{errorMessage(approve.error ?? reject.error)}</p>}
        <div className="flex flex-wrap gap-2">
          <button className="button-primary" disabled={busy || temporaryPassword.length < 12} onClick={() => approve.mutate()}>Approve and provision</button>
          <button className="button-danger" disabled={busy} onClick={() => reject.mutate()}>Reject</button>
        </div>
      </div>
    </article>
  )
}
