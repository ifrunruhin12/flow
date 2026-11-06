import assert from 'node:assert/strict'
import { Browser } from './cdp.mjs'

const origin = process.env.FLOW_BROWSER_TEST_URL
const username = process.env.FLOW_BROWSER_TEST_USERNAME
const password = process.env.FLOW_BROWSER_TEST_PASSWORD
if (!origin || !username || !password) throw new Error('Run this through TestM3LivePlayback with its disposable test fixtures.')
const browser = new Browser(process.env.FLOW_BROWSER_EXECUTABLE || 'chromium')
const audio = 'audio[data-testid="flow-audio"]'
const player = '.player-bar'
const busy = `document.querySelector('${player}')?.getAttribute('aria-busy') === 'false' && !document.querySelector('[data-testid="player-title"]')?.textContent.includes('Restoring')`
const session = (page) => page.evaluate(`fetch('/api/playback/session').then(r=>r.json())`)
const label = (name) => `button[aria-label=${JSON.stringify(name)}]`
const audioResponses = []
const conflicts = []
const foreignRequests = []
browser.on((event) => {
  if (event.method === 'Network.requestWillBeSent' && /^https?:/.test(event.params.request.url) && !event.params.request.url.startsWith(`${origin}/`)) foreignRequests.push(event.params.request.url)
  if (event.method === 'Network.responseReceived') {
    const response = event.params.response
    if (response.url.includes('/audio/') && !response.url.includes('/coverart/')) audioResponses.push(response.status)
    if (response.url.endsWith('/api/playback/session') && response.status === 409) conflicts.push(event.sessionId)
  }
})

async function login(page, name = username, secret = password) {
  await page.wait('document.querySelector("input[autocomplete=username]") !== null')
  await page.fill('input[autocomplete=username]', name)
  await page.fill('input[autocomplete=current-password]', secret)
  await page.click('form button')
  await page.wait(busy)
  assert.equal(await page.evaluate('document.cookie.includes("flow_session")'), false)
  await page.evaluate(`window.__flowOriginalAudio = document.querySelector('${audio}')`)
}

async function playAlbum(page) {
  await page.wait('[...document.querySelectorAll(".album-card button")].some(e=>e.textContent.includes("M3 Playback"))')
  await page.evaluate('[...document.querySelectorAll(".album-card button")].find(e=>e.textContent.includes("M3 Playback")).setAttribute("data-m3-album", "true")')
  await page.click('[data-m3-album]')
  await page.wait('[...document.querySelectorAll("button")].some(e=>e.textContent.trim()==="Play album" && !e.disabled)')
  await page.evaluate('[...document.querySelectorAll("button")].find(e=>e.textContent.trim()==="Play album").setAttribute("data-m3-play", "true")')
  await page.click('[data-m3-play]')
  await page.wait(`document.querySelector('${audio}').currentTime > 0 && !document.querySelector('${audio}').paused`)
}

try {
  const page = await browser.page(origin)
  await login(page)
  if (process.env.FLOW_M3_EXPECTED_SESSION) {
    const expected = JSON.parse(process.env.FLOW_M3_EXPECTED_SESSION)
    assert.deepEqual(await session(page), expected)
    await page.wait(`document.querySelector('${audio}').readyState >= 1`)
    assert.equal(await page.evaluate(`document.querySelector('${audio}').paused`), true)
    const restored = await page.evaluate(`document.querySelector('${audio}').currentTime * 1000`)
    assert.ok(Math.abs(restored - expected.positionMs) < 1000)
    console.log('PASS M3 browser: a fresh browser restored queue, volume, shuffle and position after a real flowd process restart, without autoplay.')
  } else {
    assert.deepEqual((await session(page)).queue, [])
    await page.click('button[aria-controls="playback-queue"]')
    await page.wait('document.querySelector("#playback-queue").textContent.includes("Your queue is empty")')
    await page.click(label('Close queue'))
    await playAlbum(page)
    const initial = await session(page)
    assert.equal(initial.queue.length, 2)
    assert.equal(initial.status, 'playing')
    const originalSource = await page.evaluate(`document.querySelector('${audio}').src`)
    await page.click('nav[aria-label="Music library"] button:nth-child(3)')
    await page.wait('document.querySelector("main").textContent.includes("Checkpoint")')
    assert.equal(await page.evaluate(`window.__flowOriginalAudio === document.querySelector('${audio}') && document.querySelectorAll('audio').length === 1`), true)
    assert.equal(await page.evaluate(`document.querySelector('${audio}').src`), originalSource)
    await page.wait(`document.querySelector('${audio}').currentTime >= 1`)
    await page.key('input[aria-label="Volume"]', 'ArrowLeft')
    await page.wait(busy)
    assert.ok((await session(page)).volume < 1)
    await page.click(label('Shuffle'))
    await page.wait(busy)
    assert.equal((await session(page)).shuffle, true)
    await page.click(label('Shuffle'))
    await page.wait(busy)
    await page.key('input[aria-label="Track position"]', 'Home')
    await page.wait(busy)
    await page.key('input[aria-label="Track position"]', 'ArrowRight')
    await page.wait(busy)
    await page.key('input[aria-label="Track position"]', 'ArrowRight')
    await page.wait(busy)
    assert.ok((await session(page)).positionMs >= 1000)
    await page.click(label('Pause'))
    await page.wait(busy)
    assert.equal((await session(page)).status, 'paused')
    assert.equal(await page.evaluate(`document.querySelector('${audio}').paused`), true)

    await page.click(label('Add Checkpoint to queue'))
    await page.wait(busy)
    assert.equal((await session(page)).queue.length, 3)
    await page.click('button[aria-controls="playback-queue"]')
    await page.click(label('Move queue track 3 up'))
    await page.wait(busy)
    const moved = await session(page)
    assert.equal(moved.queue[1].mediaId, initial.queue[0].mediaId)
    await page.click(label('Remove queue track 1'))
    await page.wait(busy)
    assert.equal((await session(page)).queue.length, 2)
    await page.click(label('Play queue track 2'))
    await page.wait(`document.querySelector('${audio}').currentTime > 0 && !document.querySelector('${audio}').paused`)
    await page.click(label('Close queue'))
    await page.key('input[aria-label="Track position"]', 'Home')
    await page.wait(busy)
    for (let index = 0; index < 4; index++) {
      await page.key('input[aria-label="Track position"]', 'ArrowRight')
      await page.wait(busy)
    }
    await page.wait(`fetch('/api/playback/session').then(r=>r.json()).then(s=>s.status==='paused' && s.positionMs>=4000)`)
    await page.wait(busy)
    await page.click(label('Previous track'))
    await page.wait(busy)
    assert.equal((await session(page)).queueIndex, 1)
    await page.click(label('Previous track'))
    await page.wait(busy)
    assert.equal((await session(page)).queueIndex, 0)
    await page.click(label('Play'))
    await page.wait(`document.querySelector('${audio}').currentTime > 0`)
    const playRevision = (await session(page)).revision
    await page.wait(`fetch('/api/playback/session').then(r=>r.json()).then(s=>s.revision>${playRevision} && s.positionMs>=1000 && s.status==='playing')`, 13_000)
    await page.click(label('Pause'))
    await page.wait(busy)
    const beforeRefresh = await session(page)
    await page.call('Page.reload')
    await page.wait(busy)
    await page.wait(`document.querySelector('${audio}').readyState>=1`)
    assert.deepEqual(await session(page), beforeRefresh)
    assert.equal(await page.evaluate(`document.querySelector('${audio}').paused`), true)
    assert.ok(Math.abs(await page.evaluate(`document.querySelector('${audio}').currentTime*1000`) - beforeRefresh.positionMs) < 1000)

    const secondTab = await browser.page(origin)
    await secondTab.wait(busy)
    const tabRevision = (await session(secondTab)).revision
    await page.click(label('Shuffle'))
    await page.wait(busy)
    // Dispatch without focus so the focus-refresh mechanism cannot hide the
    // intentional stale-client case this assertion is exercising.
    await secondTab.evaluate(`document.querySelector(${JSON.stringify(label('Shuffle'))}).click()`)
    await secondTab.wait(`document.querySelector('${player}').textContent.includes('another tab or device')`)
    assert.ok(conflicts.includes(secondTab.sessionId))
    assert.equal((await session(secondTab)).revision, tabRevision + 1)
    await secondTab.evaluate(`document.querySelector(${JSON.stringify(label('Shuffle'))}).click()`)
    await secondTab.wait(busy)
    await page.evaluate(`document.querySelector(${JSON.stringify(label('Shuffle'))}).click()`)
    await page.wait(`document.querySelector('${player}').textContent.includes('another tab or device')`)
    assert.ok(conflicts.includes(page.sessionId))

    // A second account has its own session and cannot change this queue.
    const own = await session(page)
    const { browserContextId } = await browser.request('Target.createBrowserContext')
    const other = await browser.page(origin, browserContextId)
    await login(other, process.env.FLOW_BROWSER_OTHER_USERNAME, process.env.FLOW_BROWSER_OTHER_PASSWORD)
    assert.deepEqual((await session(other)).queue, [])
    await other.key('input[aria-label="Volume"]', 'ArrowLeft')
    await other.wait(busy)
    assert.deepEqual(await session(page), own)
    await browser.request('Target.disposeBrowserContext', { browserContextId })

    // Failure injection exercises the browser's actual failed resource path;
    // successful playback above and retry below still decode gateway audio.
    let failed = false
    const off = browser.on((event) => {
      if (event.sessionId === page.sessionId && event.method === 'Fetch.requestPaused') {
        failed = true
        void page.call('Fetch.failRequest', { requestId: event.params.requestId, errorReason: 'ConnectionFailed' })
      }
    })
    await page.call('Fetch.enable', { patterns: [{ urlPattern: `${origin}/audio/*`, requestStage: 'Request' }] })
    await page.click('button[aria-controls="playback-queue"]')
    await page.click(label('Play queue track 2'))
    await page.wait(`document.querySelector('${player}').textContent.includes('could not be streamed')`)
    assert.equal(failed, true)
    await page.call('Fetch.disable')
    off()
    await page.wait(busy)
    await page.click(label('Close queue'))
    await page.click('.player-bar [role="alert"] + button')
    await page.wait(`document.querySelector('${audio}').currentTime>=1 && !document.querySelector('${audio}').paused`)
    await page.click(label('Pause'))
    await page.wait(busy)
    assert.ok(audioResponses.some((status) => status === 200 || status === 206))
    assert.deepEqual(foreignRequests, [])
    // Leave saved playing intent and a durable position, then close the browser.
    // The fresh browser after the process restart must still restore paused.
    await page.click('button[aria-controls="playback-queue"]')
    await page.click(label('Play queue track 1'))
    await page.wait(`document.querySelector('${audio}').currentTime>0 && !document.querySelector('${audio}').paused`)
    await page.click(label('Close queue'))
    await page.key('input[aria-label="Track position"]', 'Home')
    await page.wait(busy)
    await page.key('input[aria-label="Track position"]', 'ArrowRight')
    await page.wait(busy)
    const saved = await session(page)
    assert.equal(saved.status, 'playing')
    assert.ok(saved.positionMs > 0)
    console.log('PASS M3 browser: real decoding/progress, album playback, controls, queue edits, checkpoints, refresh, stable audio navigation, two-tab 409 reconciliation, account isolation, failed-resource handling and retry.')
    console.log(`M3_SAVED_SESSION=${JSON.stringify(saved)}`)
  }
} finally {
  await browser.close()
}
