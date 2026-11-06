import assert from 'node:assert/strict'
import { test } from 'node:test'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const api = await import(pathToFileURL(join(process.env.FLOW_PLAYER_TEST_BUILD, 'lib/api.js')))

test('playback API uses the documented routes, JSON revision, and Flow session cookie', async (t) => {
  const calls = []
  t.mock.method(globalThis, 'fetch', async (path, init) => { calls.push({ path, init }); return new Response(JSON.stringify({ revision: 1 }), { status: 200 }) })
  await api.getPlaybackSession()
  await api.updatePlaybackSession({ ifRevision: 7, positionMs: 12_000 })
  await api.clearPlaybackSession(8)
  assert.deepEqual(calls.map((call) => call.path), Array(3).fill('/api/playback/session'))
  assert.equal(calls[0].init.credentials, 'include')
  assert.equal(calls[1].init.method, 'PUT')
  assert.equal(calls[1].init.headers.get('Content-Type'), 'application/json')
  assert.deepEqual(JSON.parse(calls[1].init.body), { ifRevision: 7, positionMs: 12_000 })
  assert.equal(calls[2].init.method, 'DELETE')
  assert.deepEqual(JSON.parse(calls[2].init.body), { ifRevision: 8 })
})

test('409 exposes the authoritative session for explicit controller reconciliation', async (t) => {
  const currentSession = { queue: [], revision: 9 }
  t.mock.method(globalThis, 'fetch', async () => new Response(JSON.stringify({ error: { code: 'conflict', message: 'Playback changed.' }, currentRevision: 9, currentSession }), { status: 409 }))
  await assert.rejects(api.updatePlaybackSession({ ifRevision: 8, volume: 0.2 }), (error) => error instanceof api.ApiError && error.status === 409 && error.code === 'conflict' && assert.deepEqual(error.currentSession, currentSession) === undefined)
})
