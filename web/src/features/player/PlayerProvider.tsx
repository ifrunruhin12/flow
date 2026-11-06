import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { PlayerContext } from './context'
import { CHECKPOINT_INTERVAL_MS, PlayerController, STATE_REFRESH_INTERVAL_MS } from './controller'

export function PlayerProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient()
  const audioRef = useRef<HTMLAudioElement>(null)
  const [controller] = useState(() => new PlayerController(undefined, () => {
    queryClient.setQueryData(['session'], null)
    queryClient.removeQueries({ queryKey: ['library'] })
    queryClient.removeQueries({ queryKey: ['admin'] })
  }))

  useEffect(() => {
    const audio = audioRef.current
    if (audio === null) return
    controller.attach(audio)
    void controller.initialize()
    const checkpoint = window.setInterval(() => { void controller.checkpoint() }, CHECKPOINT_INTERVAL_MS)
    const refresh = () => { if (document.visibilityState !== 'hidden') void controller.refresh() }
    const poll = window.setInterval(refresh, STATE_REFRESH_INTERVAL_MS)
    window.addEventListener('focus', refresh)
    window.addEventListener('online', refresh)
    document.addEventListener('visibilitychange', refresh)
    return () => {
      window.clearInterval(checkpoint)
      window.clearInterval(poll)
      window.removeEventListener('focus', refresh)
      window.removeEventListener('online', refresh)
      document.removeEventListener('visibilitychange', refresh)
      controller.destroy()
    }
  }, [controller])

  return (
    <PlayerContext.Provider value={controller}>
      <audio ref={audioRef} preload="metadata" data-testid="flow-audio" />
      {children}
    </PlayerContext.Provider>
  )
}
