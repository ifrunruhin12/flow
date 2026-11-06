import { ApiError, clearPlaybackSession, errorMessage, getPlaybackSession, getSong, updatePlaybackSession } from '../../lib/api.js'
import type { PlaybackMutation, PlaybackSession, Song } from '../../lib/types.js'
import { audioSource, chooseFormat, currentOccurrence, resumePosition, type StreamFormat } from './media.js'

export const CHECKPOINT_INTERVAL_MS = 10_000
export const STATE_REFRESH_INTERVAL_MS = 15_000

export interface PlaybackAPI {
  get: () => Promise<PlaybackSession>
  update: (mutation: PlaybackMutation) => Promise<PlaybackSession>
  clear: (revision: number) => Promise<PlaybackSession>
  song: (id: string) => Promise<Song>
}

export interface PlayerSnapshot {
  session: PlaybackSession | null
  songs: Record<string, Song | null | undefined>
  loading: boolean
  pending: boolean
  playing: boolean
  transport: 'idle' | 'loading' | 'buffering' | 'playing' | 'paused' | 'error'
  positionMs: number
  error: string
  notice: string
  conflict: boolean
}

const defaultAPI: PlaybackAPI = { get: getPlaybackSession, update: updatePlaybackSession, clear: clearPlaybackSession, song: getSong }
type Patch = Omit<PlaybackMutation, 'ifRevision'>
type TransportOptions = { resume?: boolean; seek?: boolean; restore?: boolean; external?: boolean }

// One controller per authenticated provider. PostgreSQL acknowledgements own
// session state; progress and actual audio transport remain local to this tab.
export class PlayerController {
  private snapshot: PlayerSnapshot = {
    session: null, songs: {}, loading: true, pending: false, playing: false,
    transport: 'idle', positionMs: 0, error: '', notice: '', conflict: false,
  }
  private readonly listeners = new Set<() => void>()
  private audio: HTMLAudioElement | null = null
  private generation = 0
  private epoch = 0
  private chain: Promise<unknown> = Promise.resolve()
  private disposed = true
  private refreshing = false
  private sourceKey = ''
  private sourceURL = ''
  private sourceGeneration = 0
  private failedSourceGeneration = -1
  private format: StreamFormat = 'mp3'
  private baseOffsetMs = 0
  private desiredPositionMs = 0
  private previewedVolume: number | null = null
  private readonly metadataRequests = new Map<string, Promise<Song | null>>()
  private readonly events: Record<string, EventListener> = {
    timeupdate: () => this.publish({ positionMs: this.position() }),
    play: () => { this.publish({ playing: true }); this.updateMediaSession() },
    playing: () => this.publish({ playing: true, transport: 'playing' }),
    pause: () => { this.publish({ playing: false, transport: this.snapshot.transport === 'error' ? 'error' : 'paused' }); this.updateMediaSession() },
    waiting: () => this.publish({ transport: 'buffering' }),
    stalled: () => this.publish({ transport: 'buffering' }),
    canplay: () => this.publish({ transport: this.audio?.paused === false ? 'playing' : 'paused' }),
    loadedmetadata: () => this.restoreAudioPosition(),
    ended: () => { void this.ended() },
    error: () => this.audioFailed(),
  }

  constructor(private readonly api: PlaybackAPI = defaultAPI, private readonly onAuthLost?: () => void) {}

  getSnapshot = (): PlayerSnapshot => this.snapshot
  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener)
    return () => this.listeners.delete(listener)
  }

  attach(audio: HTMLAudioElement): void {
    this.audio = audio
    this.disposed = false
    this.generation++
    this.epoch++
    this.refreshing = false
    for (const [event, listener] of Object.entries(this.events)) audio.addEventListener(event, listener)
    this.installMediaSession()
  }

  async initialize(): Promise<void> {
    const generation = this.generation
    this.publish({ loading: true, pending: false, error: '' })
    try {
      const session = await this.api.get()
      if (this.disposed || generation !== this.generation) return
      await this.accept(session, { restore: true })
    } catch (error: unknown) {
      if (generation !== this.generation || this.disposed) return
      this.handleAuthError(error)
      this.publish({ loading: false, error: `Playback could not be restored. ${errorMessage(error)}` })
    }
  }

  destroy(): void {
    this.disposed = true
    this.generation++
    this.epoch++
    for (const [event, listener] of Object.entries(this.events)) this.audio?.removeEventListener(event, listener)
    this.stopSource()
    this.audio = null
    this.metadataRequests.clear()
    this.clearMediaSession()
  }

  async refresh(): Promise<void> {
    if (this.disposed || this.snapshot.pending || this.refreshing) return
    if (this.snapshot.session === null) return this.initialize()
    this.refreshing = true
    const generation = this.generation
    try {
      const latest = await this.api.get()
      if (this.disposed || generation !== this.generation || this.snapshot.pending) return
      if (latest.revision > (this.snapshot.session?.revision ?? -1)) {
        this.epoch++
        await this.accept(latest, { external: true })
      }
    } catch (error: unknown) {
      if (!this.disposed && generation === this.generation) {
        this.handleAuthError(error)
        this.publish({ notice: 'Playback state could not be refreshed. Your next action will check its revision.' })
      }
    } finally {
      if (generation === this.generation) this.refreshing = false
    }
  }

  startQueue(songs: Song[], index: number): Promise<boolean> {
    if (songs[index] === undefined) return Promise.resolve(false)
    // Metadata is local to the signed-in player; persistence contains IDs only.
    const metadata = { ...this.snapshot.songs }
    for (const song of songs) metadata[song.id] = song
    this.publish({ songs: metadata })
    return this.commit(() => ({ queue: songs.map((song) => ({ mediaId: song.id })), queueIndex: index, positionMs: 0, status: 'playing' }), { resume: true, seek: true })
  }

  play(): Promise<boolean> {
    return this.commit((session) => {
      if (session.currentMediaId === null) throw new Error('Choose a track first.')
      const duration = this.snapshot.songs[session.currentMediaId]?.durationMs ?? 0
      return { status: 'playing', positionMs: resumePosition(this.position(), duration) }
    }, { resume: true, seek: true })
  }

  pause(): Promise<boolean> {
    const positionMs = this.position()
    const occurrence = currentOccurrence(this.snapshot.session)
    this.audio?.pause()
    return this.commit((session) => {
      if (currentOccurrence(session) !== occurrence) throw new Error('The track changed before pause was saved. Pause it again.')
      return { status: 'paused', positionMs }
    })
  }

  seek(positionMs: number): Promise<boolean> {
    const occurrence = currentOccurrence(this.snapshot.session)
    return this.commit((session) => {
      if (currentOccurrence(session) !== occurrence) throw new Error('The track changed before seek was saved. Seek it again.')
      return { positionMs: Math.max(0, Math.round(positionMs)), currentMediaId: session.currentMediaId, queueIndex: session.queueIndex }
    }, { seek: true, resume: this.snapshot.playing })
  }

  next(): Promise<boolean> { return this.commit(() => ({ command: 'next', positionMs: this.position() }), { resume: this.snapshot.playing }) }
  previous(): Promise<boolean> { return this.commit(() => ({ command: 'previous', positionMs: this.position() }), { resume: this.snapshot.playing, seek: true }) }

  select(index: number): Promise<boolean> {
    const item = this.snapshot.session?.queue[index]
    if (item !== undefined && this.snapshot.songs[item.mediaId] === null) {
      const songs = { ...this.snapshot.songs }
      delete songs[item.mediaId]
      this.publish({ songs })
    }
    return this.commit((session) => {
      if (item === undefined || session.queue[index]?.addedAt !== item.addedAt || session.queue[index]?.mediaId !== item.mediaId) throw new Error('The queue changed. Select the track again.')
      return { queueIndex: index, positionMs: 0, status: 'playing' }
    }, { resume: true, seek: true })
  }

  add(mediaId: string): Promise<boolean> { return this.commit(() => ({ queueEdit: { add: [mediaId] } })) }
  remove(index: number): Promise<boolean> { return this.editOccurrence(index, (session) => ({ queueEdit: { remove: [session.queueIndex] } })) }

  move(from: number, to: number): Promise<boolean> {
    const item = this.snapshot.session?.queue[from]
    return this.commit((session) => {
      if (item === undefined || session.queue[from]?.addedAt !== item.addedAt || session.queue[from]?.mediaId !== item.mediaId || to < 0 || to >= session.queue.length) throw new Error('The queue changed. Move the track again.')
      return { queueEdit: { move: { from, to } } }
    })
  }

  private editOccurrence(index: number, build: (selected: PlaybackSession) => Patch): Promise<boolean> {
    const item = this.snapshot.session?.queue[index]
    return this.commit((session) => {
      if (item === undefined || session.queue[index]?.addedAt !== item.addedAt || session.queue[index]?.mediaId !== item.mediaId) throw new Error('The queue changed. Remove the track again.')
      return build({ ...session, queueIndex: index })
    }, { resume: this.snapshot.playing })
  }

  setVolume(volume: number): Promise<boolean> {
    return this.commit(() => ({ volume }), {}, false, () => { this.previewedVolume = null })
  }

  previewVolume(volume: number): void {
    this.previewedVolume = Math.max(0, Math.min(1, volume))
    if (this.audio !== null) this.audio.volume = this.previewedVolume
  }

  setShuffle(shuffle: boolean): Promise<boolean> { return this.commit(() => ({ shuffle })) }
  clear(): Promise<boolean> { return this.commit(() => ({}), {}, true) }

  async retry(): Promise<boolean> {
    const id = this.snapshot.session?.currentMediaId
    if (id === undefined || id === null) {
      await this.initialize()
      return false
    }
    const songs = { ...this.snapshot.songs }
    delete songs[id]
    this.publish({ songs, error: '', positionMs: this.position() })
    this.stopSource()
    return this.play()
  }

  checkpoint(): Promise<boolean> {
    const state = this.snapshot.session
    if (this.disposed || this.snapshot.pending || !this.snapshot.playing || state === null || state.status !== 'playing' || state.currentMediaId === null || this.sourceKey !== currentOccurrence(state)) return Promise.resolve(false)
    // Capture all guards together. Never substitute a later revision if a
    // queued checkpoint waits behind another operation or loses a race.
    const captured = { ifRevision: state.revision, command: 'checkpoint' as const, currentMediaId: state.currentMediaId, queueIndex: state.queueIndex, positionMs: this.position() }
    return this.commit(() => captured, {}, false, undefined, captured.ifRevision)
  }

  private ended(): Promise<boolean> {
    const state = this.snapshot.session
    if (state === null || state.currentMediaId === null || state.status !== 'playing' || this.audio?.ended !== true || this.sourceKey !== currentOccurrence(state) || !this.audio.src.endsWith(this.sourceURL) || this.audio.currentSrc !== this.audio.src) return Promise.resolve(false)
    const occurrence = currentOccurrence(state)
    const positionMs = this.position()
    this.publish({ playing: false, transport: 'paused' })
    // Earlier local writes may still be awaiting acknowledgement. After they
    // commit, sample the dispatch guards only if this exact occurrence remains
    // selected. External reconciliation increments epoch and discards this job.
    return this.commit((session) => currentOccurrence(session) !== occurrence || session.status !== 'playing' ? null : {
      command: 'ended', currentMediaId: session.currentMediaId, queueIndex: session.queueIndex, positionMs,
    }, { resume: true })
  }

  private commit(build: (session: PlaybackSession) => Patch | null, options: TransportOptions = {}, clear = false, beforeAccept?: () => void, sampledRevision?: number): Promise<boolean> {
    const epoch = this.epoch
    const generation = this.generation
    const job = this.chain.then(async () => {
      if (this.disposed || generation !== this.generation || epoch !== this.epoch) return false
      const session = this.snapshot.session
      if (session === null) { this.publish({ error: 'Playback is still loading. Try again when it is ready.' }); return false }
      this.publish({ pending: true, error: '', notice: '', conflict: false })
      try {
        const patch = build(session)
        if (patch === null) return false
        const mutation = { ...patch, ifRevision: sampledRevision ?? session.revision }
        const accepted = clear ? await this.api.clear(mutation.ifRevision) : await this.api.update(mutation)
        if (this.disposed || generation !== this.generation) return false
        beforeAccept?.()
        await this.accept(accepted, options)
        return true
      } catch (error: unknown) {
        if (this.disposed || generation !== this.generation) return false
        this.epoch++ // discard all queued actions based on the losing local state
        this.audio?.pause()
        this.previewedVolume = null
        this.handleAuthError(error)
        if (error instanceof ApiError && error.status === 409 && error.currentSession !== undefined) {
          await this.accept(error.currentSession, { external: true })
          this.publish({ conflict: true, notice: 'Playback changed in another tab or device. The latest state is restored; choose your next action.' })
        } else {
          this.publish({ error: `Playback could not be confirmed. ${errorMessage(error)}` })
          // The request might have committed before a network response was
          // lost. Read, don't replay it against an assumed revision.
          try {
            const latest = await this.api.get()
            if (!this.disposed && generation === this.generation && latest.revision >= session.revision) await this.accept(latest, { external: true })
          } catch { /* retain last acknowledged state and visible failure */ }
          this.publish({ error: `Playback could not be confirmed. ${errorMessage(error)}` })
        }
        return false
      } finally {
        if (!this.disposed && generation === this.generation) this.publish({ pending: false })
      }
    })
    this.chain = job
    return job
  }

  private async accept(session: PlaybackSession, options: TransportOptions): Promise<void> {
    const generation = this.generation
    if ((this.snapshot.session?.revision ?? -1) > session.revision) return
    const changed = currentOccurrence(session) !== this.sourceKey
    const pausedRestore = options.restore === true || options.external === true
    if (changed || pausedRestore) this.stopSource()
    this.publish({ session, loading: false, error: '', conflict: false,
      positionMs: changed || pausedRestore || options.seek === true ? session.positionMs : this.position(),
      notice: options.external === true ? 'Playback was updated by another client. Press Play to continue here.' : options.restore === true && session.currentMediaId !== null ? 'Your queue and saved position are restored. Press Play to listen.' : '',
    })
    if (this.audio !== null) this.audio.volume = this.previewedVolume ?? session.volume
    void this.prefetchQueue(session)
    if (session.currentMediaId === null) { this.publish({ playing: false, transport: 'idle', positionMs: 0 }); this.updateMediaSession(); return }
    const song = await this.metadata(session.currentMediaId)
    if (this.disposed || generation !== this.generation || currentOccurrence(this.snapshot.session) !== currentOccurrence(session)) return
    if (song === null) {
      this.audio?.pause()
      this.publish({ transport: 'error', error: 'This track is unavailable. Retry it or choose another queued track.' })
      if (!pausedRestore && session.status === 'playing') this.saveStoppedIntent(false)
      return
    }
    const position = pausedRestore ? resumePosition(session.positionMs, song.durationMs) : session.positionMs
    const resume = !pausedRestore && options.resume === true && session.status === 'playing'
    if (changed || this.sourceKey === '') {
      this.loadTrack(song, position)
    } else if (options.seek === true) {
      this.seekAudio(song, position)
    }
    if (session.status === 'paused' || pausedRestore) {
      this.audio?.pause()
      this.publish({ playing: false })
    }
    else if (resume) await this.playAudio()
    this.updateMediaSession()
  }

  private async metadata(id: string): Promise<Song | null> {
    const known = this.snapshot.songs[id]
    if (known !== undefined) return known
    const pending = this.metadataRequests.get(id)
    if (pending !== undefined) return pending
    const generation = this.generation
    const request = this.api.song(id).then((song) => {
      if (!this.disposed && generation === this.generation) this.publish({ songs: { ...this.snapshot.songs, [id]: song } })
      return song
    }).catch(() => {
      if (!this.disposed && generation === this.generation) this.publish({ songs: { ...this.snapshot.songs, [id]: null } })
      return null
    }).finally(() => {
      if (generation === this.generation) this.metadataRequests.delete(id)
    })
    this.metadataRequests.set(id, request)
    return request
  }

  private async prefetchQueue(session: PlaybackSession): Promise<void> {
    const generation = this.generation
    const ids = [...new Set(session.queue.map((item) => item.mediaId))]
    let index = 0
    await Promise.all(Array.from({ length: Math.min(ids.length, 4) }, async () => {
      while (!this.disposed && generation === this.generation) {
        const id = ids[index++]
        if (id === undefined) return
        await this.metadata(id)
      }
    }))
  }

  private loadTrack(song: Song, positionMs: number): void {
    this.stopSource()
    if (this.audio === null) return
    this.format = chooseFormat(this.audio, song)
    this.baseOffsetMs = this.format === 'original' ? 0 : Math.floor(positionMs / 1000) * 1000
    this.desiredPositionMs = this.format === 'original' ? positionMs : this.baseOffsetMs
    this.sourceKey = currentOccurrence(this.snapshot.session)
    this.sourceURL = audioSource(song.id, this.format, this.baseOffsetMs)
    this.audio.src = this.sourceURL
    this.audio.load()
    this.publish({ transport: 'loading', positionMs: this.desiredPositionMs, playing: false })
  }

  private seekAudio(song: Song, positionMs: number): void {
    if (this.format !== 'original') { this.loadTrack(song, positionMs); return }
    this.desiredPositionMs = positionMs
    this.restoreAudioPosition()
  }

  private restoreAudioPosition(): void {
    if (this.audio === null || this.sourceKey === '' || this.audio.readyState < 1) return
    if (this.format === 'original') {
      const duration = Number.isFinite(this.audio.duration) ? this.audio.duration * 1000 : 0
      const target = duration > 0 ? Math.min(this.desiredPositionMs, duration) : this.desiredPositionMs
      try { this.audio.currentTime = target / 1000 } catch { this.publish({ error: 'This audio representation could not seek. Retry the track.' }) }
    }
    this.publish({ positionMs: this.position(), transport: this.audio.paused ? 'paused' : 'playing' })
  }

  private async playAudio(): Promise<void> {
    const generation = this.sourceGeneration
    try {
      await this.audio?.play()
    } catch (error: unknown) {
      if (this.disposed || generation !== this.sourceGeneration) return
      this.audio?.pause()
      this.publish({ transport: 'error', playing: false, error: error instanceof Error && error.name === 'NotAllowedError' ? 'Your browser requires a Play gesture. Press Play to listen.' : 'Audio could not start. Retry the track.' })
      // This is a new pause intent following a committed play, not a retry of
      // the original mutation. Do not await it inside the current queue job.
      const message = this.snapshot.error
      this.saveStoppedIntent(true, message)
    }
  }

  private audioFailed(): void {
    if (this.disposed || this.sourceKey === '' || this.audio?.error === null || this.audio?.currentSrc !== this.audio?.src || this.failedSourceGeneration === this.sourceGeneration) return
    this.failedSourceGeneration = this.sourceGeneration
    this.audio?.pause()
    this.publish({ transport: 'error', playing: false, error: 'Audio could not be streamed. Retry after checking your connection, or choose another track.' })
    this.saveStoppedIntent(true, this.snapshot.error)
  }

  private saveStoppedIntent(withPosition: boolean, message = this.snapshot.error): void {
    const occurrence = currentOccurrence(this.snapshot.session)
    const sourceGeneration = this.sourceGeneration
    const positionMs = this.position()
    void this.commit((session) => currentOccurrence(session) !== occurrence || this.sourceGeneration !== sourceGeneration || this.snapshot.playing ? null : {
      status: 'paused', ...(withPosition ? { positionMs } : {}),
    }).then((saved) => {
      if (saved && !this.disposed && this.sourceGeneration === sourceGeneration &&
        currentOccurrence(this.snapshot.session) === occurrence && !this.snapshot.playing) this.publish({ error: message })
    })
  }

  private position(): number {
    if (this.sourceKey !== '' && (this.audio?.readyState ?? 0) < 1) return this.desiredPositionMs
    const time = this.audio?.currentTime
    if (this.sourceKey === '' || time === undefined || !Number.isFinite(time)) return this.snapshot.positionMs
    return Math.max(0, Math.round(this.baseOffsetMs + time * 1000))
  }

  private stopSource(): void {
    this.sourceGeneration++
    this.sourceKey = ''
    this.sourceURL = ''
    this.audio?.pause()
    this.audio?.removeAttribute('src')
    this.audio?.load()
  }

  private publish(patch: Partial<PlayerSnapshot>): void {
    if (this.disposed) return
    this.snapshot = { ...this.snapshot, ...patch }
    for (const listener of this.listeners) listener()
  }

  private handleAuthError(error: unknown): void {
    if (error instanceof ApiError && (error.status === 401 || error.code === 'account_not_active' || error.code === 'password_change_required')) {
      this.stopSource()
      this.onAuthLost?.()
    }
  }

  private installMediaSession(): void {
    if (typeof navigator === 'undefined' || !('mediaSession' in navigator)) return
    const actions: Partial<Record<MediaSessionAction, MediaSessionActionHandler>> = {
      play: () => { void this.play() }, pause: () => { void this.pause() },
      previoustrack: () => { void this.previous() }, nexttrack: () => { void this.next() },
      seekto: (details) => { if (details.seekTime !== undefined) void this.seek(details.seekTime * 1000) },
    }
    for (const [action, handler] of Object.entries(actions)) {
      try { navigator.mediaSession.setActionHandler(action as MediaSessionAction, handler) } catch { /* optional browser capability */ }
    }
  }

  private updateMediaSession(): void {
    if (typeof navigator === 'undefined' || !('mediaSession' in navigator)) return
    const id = this.snapshot.session?.currentMediaId
    const song = id === null || id === undefined ? undefined : this.snapshot.songs[id]
    navigator.mediaSession.playbackState = this.snapshot.playing ? 'playing' : 'paused'
    if (typeof MediaMetadata !== 'undefined') navigator.mediaSession.metadata = song === undefined || song === null ? null : new MediaMetadata({ title: song.title, artist: song.artist, album: song.album, artwork: song.coverUrl ? [{ src: song.coverUrl }] : [] })
  }

  private clearMediaSession(): void {
    if (typeof navigator === 'undefined' || !('mediaSession' in navigator)) return
    for (const action of ['play', 'pause', 'previoustrack', 'nexttrack', 'seekto'] as const) {
      try { navigator.mediaSession.setActionHandler(action, null) } catch { /* optional browser capability */ }
    }
    navigator.mediaSession.metadata = null
    navigator.mediaSession.playbackState = 'none'
  }
}
