import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('..', import.meta.url))
const output = mkdtempSync(join(tmpdir(), 'flow-player-tests-'))
try {
  writeFileSync(join(output, 'package.json'), '{"type":"module"}')
  const compilation = spawnSync(process.execPath, [join(root, 'node_modules/typescript/bin/tsc'), '-p', 'tsconfig.player-tests.json', '--outDir', output], { cwd: root, stdio: 'inherit' })
  if (compilation.status !== 0) process.exitCode = compilation.status ?? 1
  else {
    const result = spawnSync(process.execPath, ['tests/all.test.mjs'], { cwd: root, stdio: 'inherit', env: { ...process.env, FLOW_PLAYER_TEST_BUILD: output } })
    process.exitCode = result.status ?? 1
  }
} finally {
  rmSync(output, { recursive: true, force: true })
}
