import { useEffect, useMemo, useRef, useState } from 'react'
import type { Song } from '../../lib/types'

type StreamFormat = 'original' | 'opus' | 'mp3'

export function PlayerBar({ queue, index, onIndex }: { queue: Song[]; index: number; onIndex: (index: number) => void }) {
  const song = queue[index]
  const audioRef = useRef<HTMLAudioElement>(null)
  const [baseOffset, setBaseOffset] = useState(0)
  const [sourceRevision, setSourceRevision] = useState(0)
  const [position, setPosition] = useState(0)
  const [volume, setVolume] = useState(0.85)
  const [playing, setPlaying] = useState(false)
  const [error, setError] = useState('')
  const format = useMemo(() => chooseFormat(song), [song])

  useEffect(() => {
    setBaseOffset(0)
    setSourceRevision(0)
    setPosition(0)
    setError('')
    setPlaying(song !== undefined)
  }, [song])

  useEffect(() => {
    if (audioRef.current !== null) audioRef.current.volume = volume
  }, [song, sourceRevision, volume])

  if (song === undefined) {
    return (
      <aside className="player-bar">
        <p className="font-bold">Choose a track to start listening.</p>
        <p className="text-sm text-[var(--flow-muted)]">Playback stays in this browser for M2; persisted queues arrive in M3.</p>
      </aside>
    )
  }

  const duration = Math.max(song.durationMs / 1000, 0)
  const source = audioSource(song.id, format, baseOffset)
  const previous = () => {
    const audio = audioRef.current
    if (audio !== null && position > 3) {
      seek(0)
    } else if (index > 0) {
      onIndex(index - 1)
    }
  }
  const next = () => {
    if (index + 1 < queue.length) {
      onIndex(index + 1)
    } else {
      setPlaying(false)
    }
  }
  const toggle = () => {
    const audio = audioRef.current
    if (audio === null) return
    if (audio.paused) {
      void audio.play().catch(() => setError('Playback could not start. Try the track again.'))
    } else {
      audio.pause()
    }
  }
  function seek(target: number) {
    const bounded = Math.max(0, Math.min(target, duration))
    const audio = audioRef.current
    if (format === 'original' && audio !== null) {
      audio.currentTime = bounded
      setPosition(bounded)
      return
    }
    setBaseOffset(Math.floor(bounded))
    setPosition(bounded)
    setSourceRevision((value) => value + 1)
    setPlaying(true)
  }

  return (
    <aside className="player-bar">
      <audio
        key={`${song.id}-${sourceRevision}`}
        ref={audioRef}
        src={source}
        autoPlay
        preload="metadata"
        onPlay={() => setPlaying(true)}
        onPause={() => setPlaying(false)}
        onEnded={next}
        onError={() => setError('This representation could not be streamed.')}
        onTimeUpdate={(event) => setPosition(baseOffset + event.currentTarget.currentTime)}
      />
      <div className="min-w-0 sm:w-64">
        <p className="truncate font-black">{song.title || 'Untitled track'}</p>
        <p className="truncate text-sm text-[var(--flow-muted)]">{song.artist || song.album || 'Unknown artist'}</p>
      </div>
      <div className="flex items-center gap-2">
        <button className="player-control" disabled={index === 0 && position <= 3} onClick={previous} aria-label="Previous track">‹‹</button>
        <button className="player-control player-control-primary" onClick={toggle} aria-label={playing ? 'Pause' : 'Play'}>{playing ? 'Ⅱ' : '▶'}</button>
        <button className="player-control" disabled={index + 1 >= queue.length} onClick={next} aria-label="Next track">››</button>
      </div>
      <div className="min-w-[12rem] flex-1">
        <input
          className="w-full accent-[var(--flow-accent)]"
          type="range"
          min={0}
          max={Math.max(duration, 1)}
          step={1}
          value={Math.min(position, Math.max(duration, 1))}
          onChange={(event) => seek(Number(event.target.value))}
          aria-label="Track position"
        />
        <div className="flex justify-between text-xs text-[var(--flow-muted)]">
          <span>{formatTime(position)}</span>
          <span>{format.toUpperCase()} · {formatTime(duration)}</span>
        </div>
      </div>
      <label className="flex items-center gap-2 text-xs text-[var(--flow-muted)]">
        Volume
        <input
          className="w-24 accent-[var(--flow-accent)]"
          type="range"
          min={0}
          max={1}
          step={0.05}
          value={volume}
          onChange={(event) => {
            const nextVolume = Number(event.target.value)
            setVolume(nextVolume)
            if (audioRef.current !== null) audioRef.current.volume = nextVolume
          }}
        />
      </label>
      {error !== '' && <p className="w-full text-xs text-[var(--flow-danger)]">{error}</p>}
    </aside>
  )
}

function chooseFormat(song: Song | undefined): StreamFormat {
  if (song === undefined || typeof document === 'undefined') return 'mp3'
  const probe = document.createElement('audio')
  const contentType = song.contentType?.trim()
  if (contentType !== undefined && contentType !== '' && probe.canPlayType(contentType) !== '') return 'original'
  if (song.suffix?.toLowerCase() === 'flac' && probe.canPlayType('audio/flac') !== '') return 'original'
  if (probe.canPlayType('audio/ogg; codecs="opus"') !== '') return 'opus'
  return 'mp3'
}

function audioSource(mediaId: string, format: StreamFormat, offsetSeconds: number): string {
  const query = new URLSearchParams({ format })
  if (format !== 'original') query.set('maxBitRate', '192')
  if (format !== 'original' && offsetSeconds > 0) query.set('offsetSeconds', String(offsetSeconds))
  return `/audio/${encodeURIComponent(mediaId)}?${query.toString()}`
}

function formatTime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '0:00'
  const whole = Math.floor(seconds)
  const minutes = Math.floor(whole / 60)
  return `${minutes}:${String(whole % 60).padStart(2, '0')}`
}
