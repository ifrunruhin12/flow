// Test-only Chromium DevTools pipe client. No production dependency or server.
import { spawn } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

export class Browser {
  sequence = 0
  pending = new Map()
  listeners = new Set()
  buffer = ''
  stopped = false
  exited = false
  constructor(executable) {
    this.profile = mkdtempSync(join(tmpdir(), 'flow-m3-browser-'))
    const env = Object.fromEntries(Object.entries(process.env).filter(([name]) => !name.startsWith('FLOW_')))
    this.process = spawn(executable, ['--headless=new', '--no-sandbox', '--disable-gpu', '--mute-audio', '--remote-debugging-pipe', `--user-data-dir=${this.profile}`, 'about:blank'], { env, stdio: ['ignore', 'ignore', 'pipe', 'pipe', 'pipe'] })
    this.process.stdio[4].setEncoding('utf8')
    this.process.stdio[3].on('error', () => this.fail(new Error('Chromium closed its DevTools pipe.')))
    this.process.stdio[4].on('data', (chunk) => {
      this.buffer += chunk
      let end
      while ((end = this.buffer.indexOf('\0')) !== -1) {
        const data = this.buffer.slice(0, end)
        this.buffer = this.buffer.slice(end + 1)
        if (data === '') continue
        const message = JSON.parse(data)
        const request = this.pending.get(message.id)
        if (request !== undefined) {
          clearTimeout(request.timer)
          this.pending.delete(message.id)
          if (message.error) request.reject(new Error(`DevTools ${request.method}: ${message.error.message}`))
          else request.resolve(message.result)
        } else for (const listener of this.listeners) listener(message)
      }
    })
    // Chromium diagnostics are not test assertions and may contain local paths.
    this.process.stderr.resume()
    this.process.on('error', () => { this.exited = true; this.fail(new Error('The configured Chromium executable could not start.')) })
    this.process.on('exit', () => { this.exited = true; this.fail(new Error('Chromium exited before the browser assertions completed.')) })
  }
  fail(error) { for (const request of this.pending.values()) { clearTimeout(request.timer); request.reject(error) }; this.pending.clear() }
  request(method, params = {}, sessionId) {
    if (this.stopped || this.exited) return Promise.reject(new Error('Browser is closed.'))
    const id = ++this.sequence
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { this.pending.delete(id); reject(new Error(`DevTools ${method} timed out.`)) }, 15_000)
      this.pending.set(id, { resolve, reject, timer, method })
      this.process.stdio[3].write(`${JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) })}\0`)
    })
  }
  on(listener) { this.listeners.add(listener); return () => this.listeners.delete(listener) }
  async page(url, browserContextId) {
    const { targetId } = await this.request('Target.createTarget', { url: 'about:blank', ...(browserContextId ? { browserContextId } : {}) })
    const { sessionId } = await this.request('Target.attachToTarget', { targetId, flatten: true })
    const page = new Page(this, sessionId)
    await page.call('Page.enable')
    await page.call('Network.enable')
    await page.call('Emulation.setDeviceMetricsOverride', { width: 1280, height: 900, deviceScaleFactor: 1, mobile: false })
    await page.call('Page.navigate', { url })
    return page
  }
  async close() {
    if (this.stopped) return
    this.stopped = true
    this.fail(new Error('Browser closed.'))
    const exited = new Promise((resolve) => this.process.once('exit', resolve))
    if (!this.exited) {
      this.process.kill('SIGTERM')
      const timer = setTimeout(() => this.process.kill('SIGKILL'), 3000)
      await exited
      clearTimeout(timer)
    }
    // Chromium may let a short-lived child finish profile bookkeeping after
    // the browser process exits. Node's recursive remover supports bounded
    // retries for exactly this ENOTEMPTY/EBUSY race; cleanup must not turn a
    // successful browser assertion run into a failed integration gate.
    rmSync(this.profile, { recursive: true, force: true, maxRetries: 20, retryDelay: 100 })
  }
}

class Page {
  constructor(browser, sessionId) { this.browser = browser; this.sessionId = sessionId }
  call(method, params) { return this.browser.request(method, params, this.sessionId) }
  async evaluate(expression) {
    const result = await this.call('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true })
    if (result.exceptionDetails) throw new Error(`Browser evaluation failed: ${result.exceptionDetails.text}`)
    return result.result.value
  }
  async wait(expression, timeout = 15_000) {
    const end = Date.now() + timeout
    while (Date.now() < end) {
      if (await this.evaluate(expression)) return
      await new Promise((resolve) => setTimeout(resolve, 100))
    }
    throw new Error('Browser condition timed out.')
  }
  async click(selector) {
    const point = await this.evaluate(`(() => { const element = document.querySelector(${JSON.stringify(selector)}); if (!element || element.disabled) return null; element.scrollIntoView({block:'center'}); const r = element.getBoundingClientRect(); return {x:r.x+r.width/2,y:r.y+r.height/2}; })()`)
    if (point === null) throw new Error(`Browser control is missing or disabled: ${selector}`)
    await this.call('Input.dispatchMouseEvent', { type: 'mousePressed', button: 'left', clickCount: 1, ...point })
    await this.call('Input.dispatchMouseEvent', { type: 'mouseReleased', button: 'left', clickCount: 1, ...point })
  }
  async fill(selector, value) {
    await this.evaluate(`(() => { const element = document.querySelector(${JSON.stringify(selector)}); const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set; setter.call(element,${JSON.stringify(value)}); element.dispatchEvent(new Event('input',{bubbles:true})); })()`)
  }
  async key(selector, key) {
    await this.evaluate(`document.querySelector(${JSON.stringify(selector)}).focus()`)
    const codes = { Home: 36, End: 35, ArrowLeft: 37, ArrowRight: 39 }
    const params = { key, code: key, windowsVirtualKeyCode: codes[key], nativeVirtualKeyCode: codes[key] }
    await this.call('Input.dispatchKeyEvent', { type: 'rawKeyDown', ...params })
    await this.call('Input.dispatchKeyEvent', { type: 'keyUp', ...params })
  }
}
