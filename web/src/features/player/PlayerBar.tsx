import { useEffect, useRef, useState } from 'react'
import { usePlayer } from './context'
import { currentOccurrence, formatTime } from './media'

export function PlayerBar() {
  const player = usePlayer()
  const [queueOpen, setQueueOpen] = useState(false)
  const session = player.session
  const song = session?.currentMediaId === null || session?.currentMediaId === undefined ? undefined : player.songs[session.currentMediaId]
  const durationMs = song?.durationMs ?? 0
  const hasTrack = session?.currentMediaId !== null && session?.currentMediaId !== undefined
  const disabled = player.loading || player.pending || !hasTrack

  return (
    <>
      {queueOpen && <QueuePanel onClose={() => setQueueOpen(false)} />}
      <aside className="player-bar" aria-label="Music player" aria-busy={player.pending}>
        <div className="flex min-w-0 items-center gap-3 sm:w-64">
          {song?.coverUrl ? <img className="artwork" src={song.coverUrl} alt="Album artwork" /> : <div className="artwork grid place-items-center text-xl text-[var(--flow-muted)]" aria-hidden="true">♫</div>}
          <div className="min-w-0">
            <p className="truncate font-black" data-testid="player-title">{player.loading ? 'Restoring your player…' : song?.title || (hasTrack ? 'Track unavailable' : 'Your listening space')}</p>
            <p className="truncate text-sm text-[var(--flow-muted)]">{song?.artist || (hasTrack ? 'Choose another track or retry' : 'Choose music to start a queue')}</p>
            {(player.transport === 'loading' || player.transport === 'buffering') && <p className="text-xs text-[var(--flow-accent-strong)]" role="status">{player.transport === 'loading' ? 'Loading audio…' : 'Buffering…'}</p>}
          </div>
        </div>
        <div className="flex items-center gap-2">
          <button className="player-control" disabled={disabled} onClick={() => { void player.controller.previous() }} aria-label="Previous track">‹‹</button>
          <button className="player-control player-control-primary" disabled={disabled} onClick={() => { void (player.playing ? player.controller.pause() : player.controller.play()) }} aria-label={player.playing ? 'Pause' : 'Play'}>{player.playing ? 'Ⅱ' : '▶'}</button>
          <button className="player-control" disabled={disabled} onClick={() => { void player.controller.next() }} aria-label="Next track">››</button>
          <button className={`player-control ${session?.shuffle === true ? 'player-control-primary' : ''}`} disabled={player.loading || player.pending} onClick={() => { void player.controller.setShuffle(session?.shuffle !== true) }} aria-label="Shuffle" aria-pressed={session?.shuffle ?? false}>⤨</button>
        </div>
        <div className="min-w-[12rem] flex-1">
          <CommitSlider key={currentOccurrence(session)} label="Track position" value={player.positionMs} max={Math.max(durationMs, player.positionMs, 1)} step={1000} disabled={disabled} onCommit={(value) => { void player.controller.seek(value) }} />
          <div className="flex justify-between text-xs text-[var(--flow-muted)]">
            <span data-testid="player-position">{formatTime(player.positionMs)}</span>
            <span>{formatTime(durationMs)}</span>
          </div>
        </div>
        <label className="flex items-center gap-2 text-xs text-[var(--flow-muted)]">
          Volume
          <span className="w-24"><CommitSlider label="Volume" value={session?.volume ?? 1} max={1} step={0.01} disabled={player.loading || player.pending} onPreview={(value) => player.controller.previewVolume(value)} onCommit={(value) => { void player.controller.setVolume(value) }} /></span>
        </label>
        <button className="button-secondary" aria-expanded={queueOpen} aria-controls="playback-queue" onClick={() => setQueueOpen(!queueOpen)}>Queue · {session?.queue.length ?? 0}</button>
        {player.pending && <span className="text-xs text-[var(--flow-muted)]" role="status">Saving…</span>}
        {player.notice !== '' && <p className={`w-full text-xs ${player.conflict ? 'text-[var(--flow-warning)]' : 'text-[var(--flow-muted)]'}`} role="status">{player.notice}</p>}
        {player.error !== '' && <div className="flex w-full flex-wrap items-center gap-3"><p className="text-xs text-[var(--flow-danger)]" role="alert">{player.error}</p><button className="button-secondary text-xs" disabled={player.pending} onClick={() => { void player.controller.retry() }}>Retry playback</button></div>}
      </aside>
    </>
  )
}

function QueuePanel({ onClose }: { onClose: () => void }) {
  const { controller, session, songs, pending, loading, playing } = usePlayer()
  return (
    <section id="playback-queue" className="queue-panel panel" aria-label="Playback queue">
      <header className="mb-5 flex items-center justify-between gap-3">
        <div><h2 className="text-xl font-black">Your queue</h2><p className="text-xs text-[var(--flow-muted)]">{session?.shuffle === true ? 'Shuffle is on; the list keeps its original order.' : 'Saved for your account across devices.'}</p></div>
        <div className="flex items-center gap-2"><button className="button-secondary text-xs" disabled={pending || loading || session?.queue.length === 0} onClick={() => { void controller.clear() }}>Clear queue</button><button className="player-control" onClick={onClose} aria-label="Close queue">×</button></div>
      </header>
      {session?.queue.length === 0 && <p className="py-8 text-center text-[var(--flow-muted)]">Your queue is empty. Add music from the library.</p>}
      <ol className="divide-y divide-[var(--flow-border)]">
        {session?.queue.map((item, index) => {
          const song = songs[item.mediaId]
          const current = index === session.queueIndex
          return (
            <li key={`${item.mediaId}-${item.addedAt}-${index}`} className={`song-row ${current ? 'queue-current' : ''}`} aria-current={current ? 'true' : undefined}>
              <button className="player-control" disabled={pending} onClick={() => { void controller.select(index) }} aria-label={`Play queue track ${index + 1}`}>{current && playing ? '♫' : '▶'}</button>
              <span className="min-w-0 flex-1"><strong className="block truncate">{song?.title || (song === null ? 'Unavailable track' : 'Loading track…')}</strong><span className="block truncate text-xs text-[var(--flow-muted)]">{song?.artist || item.mediaId}{current ? ' · Current track' : ''}</span></span>
              <button className="player-control" disabled={pending || index === 0} onClick={() => { void controller.move(index, index - 1) }} aria-label={`Move queue track ${index + 1} up`}>↑</button>
              <button className="player-control" disabled={pending || index + 1 >= session.queue.length} onClick={() => { void controller.move(index, index + 1) }} aria-label={`Move queue track ${index + 1} down`}>↓</button>
              <button className="player-control" disabled={pending} onClick={() => { void controller.remove(index) }} aria-label={`Remove queue track ${index + 1}`}>×</button>
            </li>
          )
        })}
      </ol>
    </section>
  )
}

// Preview during a gesture; commit once on release/keyboard completion or blur.
// This avoids a database write for every slider progress event.
function CommitSlider({ label, value, max, step, disabled, onPreview, onCommit }: { label: string; value: number; max: number; step: number; disabled: boolean; onPreview?: (value: number) => void; onCommit: (value: number) => void }) {
  const [draft, setDraft] = useState(value)
  const dirty = useRef(false)
  useEffect(() => { if (!dirty.current) setDraft(value) }, [value])
  const commit = (next: number) => {
    if (!dirty.current) return
    dirty.current = false
    onCommit(next)
  }
  return <input className="w-full accent-[var(--flow-accent)]" type="range" min={0} max={max} step={step} value={Math.min(draft, max)} disabled={disabled} aria-label={label}
    onChange={(event) => { dirty.current = true; const next = Number(event.target.value); setDraft(next); onPreview?.(next) }}
    onPointerUp={(event) => commit(Number(event.currentTarget.value))}
    onKeyUp={(event) => { if (['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown', 'Home', 'End', 'PageUp', 'PageDown'].includes(event.key)) commit(Number(event.currentTarget.value)) }}
    onBlur={(event) => commit(Number(event.currentTarget.value))} />
}
