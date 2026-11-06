import { createContext, useContext, useSyncExternalStore } from 'react'
import type { PlayerController } from './controller'

export const PlayerContext = createContext<PlayerController | null>(null)

export function usePlayer() {
  const controller = useContext(PlayerContext)
  if (controller === null) throw new Error('The player must be used inside its authenticated provider.')
  const snapshot = useSyncExternalStore(controller.subscribe, controller.getSnapshot)
  return { controller, ...snapshot }
}
