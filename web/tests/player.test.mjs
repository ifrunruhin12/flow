import assert from 'node:assert/strict'
import { test } from 'node:test'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const build = process.env.FLOW_PLAYER_TEST_BUILD
const { PlayerController, CHECKPOINT_INTERVAL_MS, STATE_REFRESH_INTERVAL_MS } = await import(pathToFileURL(join(build, 'features/player/controller.js')))
const { ApiError } = await import(pathToFileURL(join(build, 'lib/api.js')))
const { audioSource, chooseFormat, resumePosition } = await import(pathToFileURL(join(build, 'features/player/media.js')))

const song = (id) => ({ id, title: `Song ${id}`, artist: 'Flow artist', album: 'Flow album', durationMs: 120_000, contentType: 'audio/flac', suffix: 'flac', starred: false })
const state = (ids = [], extra = {}) => ({ queue: ids.map((mediaId, index) => ({ mediaId, addedAt: `2026-11-06T00:00:${String(index).padStart(2, '0')}Z` })), queueIndex: 0, currentMediaId: ids[0] ?? null, positionMs: 0, volume: 1, shuffle: false, status: 'paused', revision: 0, updatedAt: '2026-11-06T00:00:00Z', ...extra })
const copy = (value) => structuredClone(value)
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }
const settle = async () => { for (let index = 0; index < 8; index++) await new Promise((resolve) => setImmediate(resolve)) }

// Transport double: models the observable media API, not the controller's
// implementation. Browser tests separately require real decoding and progress.
class AudioDouble extends EventTarget {
  src = ''
  currentTime = 0
  readyState = 0
  duration = 120
  volume = 1
  paused = true
  ended = false
  error = null
  playCalls = 0
  loads = []
  playError = null
  supportsOriginal = true
  get currentSrc() { return this.src }
  canPlayType(type) { return this.supportsOriginal && type === 'audio/flac' ? 'probably' : type.includes('opus') ? 'probably' : '' }
  load() { this.loads.push(this.src); this.currentTime = 0; this.readyState = 0; this.ended = false; this.error = null }
  removeAttribute(name) { if (name === 'src') this.src = '' }
  metadata() { this.readyState = 1; this.dispatchEvent(new Event('loadedmetadata')) }
  async play() {
    this.playCalls++
    if (this.playError !== null) throw this.playError
    if (this.error !== null) throw new DOMException('The source failed', 'NotSupportedError')
    this.paused = false
    this.ended = false
    this.dispatchEvent(new Event('play'))
    this.dispatchEvent(new Event('playing'))
  }
  pause() { if (!this.paused) { this.paused = true; this.dispatchEvent(new Event('pause')) } }
  progress(seconds) { this.currentTime = seconds; this.readyState = 1; this.dispatchEvent(new Event('timeupdate')) }
  finish() { this.currentTime = this.duration; this.ended = true; this.paused = true; this.dispatchEvent(new Event('ended')) }
}

// The double supplies explicit server snapshots and enforces revision CAS.
// Queue semantics themselves are asserted by the Go service/PostgreSQL suites.
class ServerDouble {
  calls = []
  gets = 0
  mediaCalls = []
  nextReply = null
  updateGate = null
  getGate = null
  mediaErrors = new Map()
  constructor(session = state()) { this.session = copy(session) }
  get = async () => { this.gets++; return this.getGate === null ? copy(this.session) : this.getGate.promise }
  song = async (id) => { this.mediaCalls.push(id); if (this.mediaErrors.has(id)) throw this.mediaErrors.get(id); return song(id) }
  update = async (mutation) => {
    this.calls.push(copy(mutation))
    if (this.updateGate !== null) await this.updateGate.promise
    if (mutation.ifRevision !== this.session.revision) throw new ApiError(409, 'conflict', 'Playback changed.', copy(this.session))
    const next = this.nextReply === null ? { ...this.session, ...copy(mutation) } : copy(this.nextReply)
    for (const key of ['ifRevision', 'command', 'queueEdit']) delete next[key]
    if (mutation.queue !== undefined) {
      next.queue = mutation.queue.map((item, index) => ({ mediaId: item.mediaId, addedAt: `2026-11-06T01:00:${String(index).padStart(2, '0')}Z` }))
      next.currentMediaId = next.queue[next.queueIndex]?.mediaId ?? null
    } else if (mutation.queueIndex !== undefined && mutation.command === undefined) next.currentMediaId = next.queue[next.queueIndex]?.mediaId ?? null
    next.revision = this.session.revision + 1
    next.updatedAt = `2026-11-06T02:00:${String(next.revision).padStart(2, '0')}Z`
    this.nextReply = null
    this.session = next
    return copy(next)
  }
  clear = async (revision) => {
    this.calls.push({ clear: true, ifRevision: revision })
    if (revision !== this.session.revision) throw new ApiError(409, 'conflict', 'Playback changed.', copy(this.session))
    this.session = state([], { revision: revision + 1, volume: this.session.volume, shuffle: this.session.shuffle })
    return copy(this.session)
  }
}

async function setup(t, initial = state(), configure) {
  const api = new ServerDouble(initial)
  configure?.(api)
  const audio = new AudioDouble()
  const player = new PlayerController(api)
  player.attach(audio)
  await player.initialize()
  t.after(() => player.destroy())
  if (audio.src !== '') audio.metadata()
  return { api, audio, player }
}

test('default initialization is empty, paused, and does not open audio', async (t) => {
  const { player, api, audio } = await setup(t)
  assert.equal(api.gets, 1)
  assert.deepEqual(player.getSnapshot().session, state())
  assert.equal(player.getSnapshot().transport, 'idle')
  assert.equal(audio.src, '')
  assert.equal(audio.playCalls, 0)
})

test('restore keeps the queue, saved position, volume and shuffle without autoplay', async (t) => {
  const saved = state(['a', 'b'], { positionMs: 42_500, volume: 0.25, shuffle: true, status: 'playing', revision: 9 })
  const { player, audio, api } = await setup(t, saved)
  assert.deepEqual(player.getSnapshot().session, saved)
  assert.equal(audio.currentTime, 42.5)
  assert.equal(audio.volume, 0.25)
  assert.equal(audio.playCalls, 0)
  assert.equal(player.getSnapshot().playing, false)
  assert.match(player.getSnapshot().notice, /Press Play/)
  assert.equal(api.calls.length, 0)
})

test('resume resets at a known duration and has no age cutoff', async (t) => {
  const { audio } = await setup(t, state(['a'], { positionMs: 120_000, updatedAt: '2020-01-01T00:00:00Z' }))
  assert.equal(audio.currentTime, 0)
  assert.equal(resumePosition(42_000, 0), 42_000)
  assert.equal(resumePosition(119_000, 120_000), 119_000)
})

test('play commits first; pause persists the actual audio position', async (t) => {
  const { player, audio, api } = await setup(t, state(['a']))
  const gate = deferred()
  api.updateGate = gate
  const playing = player.play()
  await settle()
  assert.equal(audio.playCalls, 0)
  assert.equal(player.getSnapshot().pending, true)
  gate.resolve()
  assert.equal(await playing, true)
  assert.equal(audio.playCalls, 1)
  audio.progress(18.25)
  assert.equal(await player.pause(), true)
  assert.equal(api.calls.at(-1).positionMs, 18_250)
  assert.equal(api.session.status, 'paused')
  assert.equal(audio.paused, true)
})

test('seek saves before moving transport, and a paused seek stays paused', async (t) => {
  const { player, audio, api } = await setup(t, state(['a']))
  const gate = deferred()
  api.updateGate = gate
  const seeking = player.seek(32_000)
  await settle()
  assert.equal(audio.currentTime, 0)
  assert.equal(api.calls[0].currentMediaId, 'a')
  gate.resolve()
  assert.equal(await seeking, true)
  assert.equal(audio.currentTime, 32)
  assert.equal(audio.playCalls, 0)
  await player.play()
  await player.seek(64_000)
  assert.equal(audio.currentTime, 64)
  assert.equal(player.getSnapshot().playing, true)
})

test('a track transition cancels the old gateway source and keeps the same element', async (t) => {
  const { player, audio, api } = await setup(t, state(['a', 'b']))
  await player.play()
  audio.progress(12)
  const loads = audio.loads.length
  api.nextReply = state(['a', 'b'], { queueIndex: 1, currentMediaId: 'b', status: 'playing' })
  await player.next()
  assert.deepEqual(api.calls.at(-1), { command: 'next', positionMs: 12_000, ifRevision: 1 })
  assert.equal(audio.src, '/audio/b?format=original')
  assert.ok(audio.loads.slice(loads).includes(''))
  assert.equal(player.getSnapshot().positionMs, 0)
  assert.equal(audio.playCalls, 2)
})

test('previous restarts without reloading an unchanged original source', async (t) => {
  const { player, audio, api } = await setup(t, state(['a']))
  await player.play()
  audio.progress(8)
  const loads = audio.loads.length
  api.nextReply = { ...api.session, positionMs: 0 }
  await player.previous()
  assert.equal(audio.currentTime, 0)
  assert.equal(audio.loads.length, loads)
  assert.equal(api.calls.at(-1).command, 'previous')
})

test('listing playback sends IDs only and selects the requested album occurrence', async (t) => {
  const { player, api, audio } = await setup(t)
  await player.startQueue([song('a'), song('b')], 1)
  assert.deepEqual(api.calls[0], { queue: [{ mediaId: 'a' }, { mediaId: 'b' }], queueIndex: 1, positionMs: 0, status: 'playing', ifRevision: 0 })
  assert.equal(audio.src, '/audio/b?format=original')
  assert.equal(audio.playCalls, 1)
})

test('queue append/reorder/remove persist through one controller and preserve current occurrence', async (t) => {
  const { player, api, audio } = await setup(t, state(['a', 'b']))
  await player.play()
  audio.progress(9)
  const loads = audio.loads.length
  api.nextReply = state(['a', 'b', 'c'], { status: 'playing' })
  await player.add('c')
  assert.deepEqual(api.calls.at(-1).queueEdit, { add: ['c'] })
  const reordered = [api.session.queue[1], api.session.queue[0], api.session.queue[2]]
  api.nextReply = { ...api.session, queue: reordered, queueIndex: 1 }
  await player.move(0, 1)
  assert.deepEqual(api.calls.at(-1).queueEdit, { move: { from: 0, to: 1 } })
  api.nextReply = { ...api.session, queue: reordered.slice(1), queueIndex: 0 }
  await player.remove(0)
  assert.deepEqual(api.calls.at(-1).queueEdit, { remove: [0] })
  assert.equal(audio.loads.length, loads)
  assert.equal(audio.currentTime, 9)
  assert.equal(player.getSnapshot().playing, true)
})

test('queue selection resets position; clear keeps volume/shuffle and stops audio', async (t) => {
  const { player, api, audio } = await setup(t, state(['a', 'b'], { volume: 0.3, shuffle: true }))
  await player.select(1)
  assert.equal(api.session.currentMediaId, 'b')
  assert.equal(api.session.positionMs, 0)
  await player.clear()
  assert.deepEqual(api.session.queue, [])
  assert.equal(api.session.volume, 0.3)
  assert.equal(api.session.shuffle, true)
  assert.equal(audio.src, '')
  assert.equal(audio.paused, true)
})

test('volume previews do not write; one committed value and shuffle use CAS', async (t) => {
  const { player, api, audio } = await setup(t, state(['a']))
  player.previewVolume(0.3)
  player.previewVolume(0.4)
  assert.equal(api.calls.length, 0)
  assert.equal(audio.volume, 0.4)
  await player.setVolume(0.4)
  await player.setShuffle(true)
  assert.deepEqual(api.calls, [{ volume: 0.4, ifRevision: 0 }, { shuffle: true, ifRevision: 1 }])
  assert.equal(api.session.shuffle, true)
})

test('progress is render-only; periodic checkpoints use sampled revision and identity', async (t) => {
  assert.equal(CHECKPOINT_INTERVAL_MS, 10_000)
  assert.equal(STATE_REFRESH_INTERVAL_MS, 15_000)
  const { player, api, audio } = await setup(t, state(['a']))
  assert.equal(await player.checkpoint(), false)
  await player.play()
  for (let index = 1; index <= 20; index++) audio.progress(index / 2)
  assert.equal(api.calls.length, 1)
  await player.checkpoint()
  assert.deepEqual(api.calls.at(-1), { command: 'checkpoint', currentMediaId: 'a', queueIndex: 0, positionMs: 10_000, ifRevision: 1 })
  assert.equal(api.session.positionMs, 10_000)
})

test('buffering and loading are distinct from actual transport', async (t) => {
  const { player, audio } = await setup(t, state(['a']))
  await player.play()
  audio.dispatchEvent(new Event('waiting'))
  assert.equal(player.getSnapshot().transport, 'buffering')
  audio.dispatchEvent(new Event('playing'))
  assert.equal(player.getSnapshot().transport, 'playing')
  audio.dispatchEvent(new Event('stalled'))
  assert.equal(player.getSnapshot().transport, 'buffering')
})

test('autoplay rejection is visible and persists a new paused intent', async (t) => {
  const { player, audio, api } = await setup(t, state(['a']))
  audio.playError = new DOMException('Gesture required', 'NotAllowedError')
  await player.play()
  await settle()
  assert.equal(player.getSnapshot().playing, false)
  assert.match(player.getSnapshot().error, /Play gesture/)
  assert.equal(api.session.status, 'paused')
  audio.playError = null
  await player.play()
  assert.equal(player.getSnapshot().playing, true)
})

test('gateway errors pause locally, preserve the queue and offer an explicit retry', async (t) => {
  const { player, audio, api } = await setup(t, state(['a', 'b']))
  await player.play()
  audio.progress(23)
  audio.error = { code: 2 }
  audio.dispatchEvent(new Event('error'))
  await settle()
  assert.equal(player.getSnapshot().playing, false)
  assert.match(player.getSnapshot().error, /could not be streamed/)
  assert.equal(api.session.status, 'paused')
  assert.equal(api.session.queue.length, 2)
  assert.equal(api.session.positionMs, 23_000)
  const loads = audio.loads.length
  await player.retry()
  assert.equal(player.getSnapshot().playing, true)
  assert.ok(audio.loads.length > loads)
})

test('unavailable restored media does not crash and can be explicitly retried', async (t) => {
  const { player, api, audio } = await setup(t, state(['gone', 'b']), (server) => server.mediaErrors.set('gone', new ApiError(404, 'not_found', 'Gone')))
  assert.match(player.getSnapshot().error, /unavailable/)
  assert.equal(audio.src, '')
  assert.equal(player.getSnapshot().session.queue.length, 2)
  api.mediaErrors.delete('gone')
  await player.retry()
  assert.equal(audio.src, '/audio/gone?format=original')
})

test('a stale checkpoint adopts the authoritative track, pauses, and is never replayed', async (t) => {
  const { player, api, audio } = await setup(t, state(['a', 'b']))
  await player.play()
  audio.progress(16)
  api.session = state(['a', 'b'], { queueIndex: 1, currentMediaId: 'b', positionMs: 30_000, revision: 2, status: 'playing' })
  assert.equal(await player.checkpoint(), false)
  assert.equal(api.calls.length, 2)
  assert.equal(api.calls.at(-1).ifRevision, 1)
  assert.equal(player.getSnapshot().session.currentMediaId, 'b')
  assert.equal(player.getSnapshot().conflict, true)
  assert.equal(audio.paused, true)
  assert.equal(audio.playCalls, 1)
  assert.equal(await player.checkpoint(), false)
})

test('a conflict cancels queued stale actions; a fresh gesture uses the new revision', async (t) => {
  const { player, api } = await setup(t, state(['a']))
  const gate = deferred()
  api.updateGate = gate
  const first = player.setVolume(0.2)
  const stale = player.setShuffle(true)
  await settle()
  api.session = { ...api.session, revision: 3, volume: 0.7 }
  gate.resolve()
  assert.equal(await first, false)
  assert.equal(await stale, false)
  assert.equal(api.calls.length, 1)
  await player.setShuffle(true)
  assert.equal(api.calls.at(-1).ifRevision, 3)
  assert.equal(api.session.volume, 0.7)
})

test('two tabs with one server session cannot overwrite the same revision', async (t) => {
  const { player: first, api } = await setup(t, state(['a']))
  const second = new PlayerController(api)
  const audio = new AudioDouble()
  second.attach(audio)
  await second.initialize()
  t.after(() => second.destroy())
  const results = await Promise.all([first.setVolume(0.2), second.setVolume(0.8)])
  assert.equal(results.filter(Boolean).length, 1)
  assert.equal(api.session.revision, 1)
  const loser = results[0] ? second : first
  assert.equal(loser.getSnapshot().conflict, true)
  assert.deepEqual(loser.getSnapshot().session, api.session)
})

test('refresh ignores an older in-flight GET and adopts newer external state without autoplay', async (t) => {
  const { player, api, audio } = await setup(t, state(['a', 'b']))
  await player.play()
  const old = copy(api.session)
  const gate = deferred()
  api.getGate = gate
  const refresh = player.refresh()
  await player.setVolume(0.2)
  gate.resolve(old)
  await refresh
  assert.equal(player.getSnapshot().session.revision, 2)
  api.getGate = null
  api.session = state(['a', 'b'], { queueIndex: 1, currentMediaId: 'b', revision: 3, status: 'playing' })
  await player.refresh()
  assert.equal(player.getSnapshot().session.revision, 3)
  assert.equal(audio.paused, true)
  assert.equal(audio.playCalls, 1)
  assert.match(player.getSnapshot().notice, /another client/)
})

test('an uncertain network result is read back, never retried or silently claimed saved', async (t) => {
  const { player, api, audio } = await setup(t, state(['a']))
  await player.play()
  api.update = async (mutation) => { api.calls.push(copy(mutation)); api.session = { ...api.session, revision: api.session.revision + 1, volume: mutation.volume }; throw new TypeError('Network response lost') }
  assert.equal(await player.setVolume(0.6), false)
  assert.equal(api.calls.length, 2)
  assert.equal(api.gets, 2)
  assert.equal(player.getSnapshot().session.volume, 0.6)
  assert.equal(audio.paused, true)
  assert.match(player.getSnapshot().error, /could not be confirmed/)
})

test('natural end persists the sampled position and advances through the server', async (t) => {
  const { player, api, audio } = await setup(t, state(['a', 'b']))
  await player.play()
  api.nextReply = state(['a', 'b'], { queueIndex: 1, currentMediaId: 'b', status: 'playing' })
  audio.finish()
  await settle()
  assert.deepEqual(api.calls.at(-1), { command: 'ended', currentMediaId: 'a', queueIndex: 0, positionMs: 120_000, ifRevision: 1 })
  assert.equal(audio.src, '/audio/b?format=original')
  api.nextReply = { ...api.session, status: 'paused', positionMs: 120_000 }
  audio.finish()
  await settle()
  assert.equal(api.session.status, 'paused')
  assert.equal(api.session.positionMs, 120_000)
  assert.equal(audio.paused, true)
  assert.equal(player.getSnapshot().playing, false)
})

test('natural end after an in-flight local command must still be durably advanced', async (t) => {
  const { player, api, audio } = await setup(t, state(['a', 'b']))
  await player.play()
  const gate = deferred()
  api.updateGate = gate
  const update = api.update
  api.update = async (mutation) => {
    if (mutation.command === 'ended') api.nextReply = state(['a', 'b'], { queueIndex: 1, currentMediaId: 'b', status: 'playing', volume: 0.3 })
    return update(mutation)
  }
  const volume = player.setVolume(0.3)
  await settle()
  audio.finish()
  gate.resolve()
  await volume
  await settle()
  assert.equal(api.session.currentMediaId, 'b')
  assert.equal(api.calls.at(-1).ifRevision, 2)
  assert.equal(player.getSnapshot().conflict, false)
})

test('destroy cancels the source, removes listeners and discards late initialization', async (t) => {
  const api = new ServerDouble(state(['a']))
  const gate = deferred()
  api.getGate = gate
  const audio = new AudioDouble()
  const player = new PlayerController(api)
  player.attach(audio)
  const opening = player.initialize()
  player.destroy()
  gate.resolve(api.session)
  await opening
  assert.equal(audio.src, '')
  const before = player.getSnapshot()
  audio.progress(4)
  assert.equal(player.getSnapshot(), before)
  t.after(() => player.destroy())
})

test('format negotiation and transcode seeking use only the authenticated Flow gateway', async (t) => {
  const { player, audio, api } = await setup(t, state(['a']))
  audio.supportsOriginal = false
  api.nextReply = state(['b'])
  await player.startQueue([song('b')], 0)
  audio.metadata()
  await player.seek(45_789)
  assert.equal(audio.src, '/audio/b?format=opus&maxBitRate=192&offsetSeconds=45')
  assert.equal(player.getSnapshot().positionMs, 45_000)
  audio.progress(3)
  assert.equal(player.getSnapshot().positionMs, 48_000)
  assert.equal(chooseFormat({ canPlayType: () => '' }, song('a')), 'mp3')
  assert.equal(audioSource('abc', 'original', 50_000), '/audio/abc?format=original')
})

test('empty play/seek are safe and an initialization error remains recoverable', async (t) => {
  const { player, api } = await setup(t)
  assert.equal(await player.play(), false)
  assert.equal(api.calls.length, 0)
  const another = new PlayerController({ ...api, get: async () => { throw new Error('Unavailable') } })
  another.attach(new AudioDouble())
  await another.initialize()
  assert.equal(another.getSnapshot().loading, false)
  assert.match(another.getSnapshot().error, /could not be restored/)
  another.destroy()
})

test('a delayed end event cannot advance a locally selected new occurrence', async (t) => {
  const { player, api, audio } = await setup(t, state(['a', 'b']))
  await player.play()
  const gate = deferred()
  api.updateGate = gate
  const selecting = player.select(1)
  await settle()
  audio.finish()
  gate.resolve()
  await selecting
  await settle()
  assert.equal(api.calls.length, 2)
  assert.equal(api.session.currentMediaId, 'b')
  assert.equal(player.getSnapshot().error, '')
})

test('a canceled source error cannot pause its newly loaded replacement', async (t) => {
  const { player, api, audio } = await setup(t, state(['a', 'b']))
  await player.play()
  await player.select(1)
  audio.dispatchEvent(new Event('error'))
  await settle()
  assert.equal(api.calls.length, 2)
  assert.equal(player.getSnapshot().playing, true)
  assert.equal(player.getSnapshot().error, '')
})

test('a stopped-intent conflict retains reconciliation instead of restoring the old stream error', async (t) => {
  const { player, api, audio } = await setup(t, state(['a']))
  await player.play()
  api.session = { ...api.session, revision: 2, volume: 0.4 }
  audio.error = { code: 2 }
  audio.dispatchEvent(new Event('error'))
  await settle()
  assert.equal(player.getSnapshot().conflict, true)
  assert.equal(player.getSnapshot().session.volume, 0.4)
  assert.equal(player.getSnapshot().error, '')
  assert.match(player.getSnapshot().notice, /another tab or device/)
  assert.equal(api.calls.length, 2)
})

test('checkpoints are skipped while a command is pending and while locally paused', async (t) => {
  const { player, api, audio } = await setup(t, state(['a']))
  await player.play()
  audio.progress(10)
  const gate = deferred()
  api.updateGate = gate
  const volume = player.setVolume(0.5)
  await settle()
  assert.equal(await player.checkpoint(), false)
  assert.equal(api.calls.length, 2)
  gate.resolve()
  await volume
  await player.pause()
  assert.equal(await player.checkpoint(), false)
  assert.equal(api.calls.length, 3)
})

test('authentication loss is reported by refresh without retrying unauthorized mutations', async (t) => {
  const api = new ServerDouble(state(['a']))
  let lost = 0
  const player = new PlayerController(api, () => { lost++ })
  const audio = new AudioDouble()
  player.attach(audio)
  await player.initialize()
  await player.play()
  api.get = async () => { throw new ApiError(401, 'unauthenticated', 'Revoked') }
  await player.refresh()
  assert.equal(lost, 1)
  assert.equal(api.calls.length, 1)
  assert.equal(audio.src, '')
  assert.equal(audio.paused, true)
  player.destroy()
  t.after(() => player.destroy())
})
