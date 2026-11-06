import type { PlaybackSession, Song } from '../../lib/types.js'

export type StreamFormat = 'original' | 'opus' | 'mp3'

export function chooseFormat(audio: Pick<HTMLAudioElement, 'canPlayType'>, song: Song): StreamFormat {
  const contentType = song.contentType?.trim()
  if (contentType !== undefined && contentType !== '' && audio.canPlayType(contentType) !== '') return 'original'
  if (song.suffix?.toLowerCase() === 'flac' && audio.canPlayType('audio/flac') !== '') return 'original'
  if (audio.canPlayType('audio/ogg; codecs="opus"') !== '') return 'opus'
  return 'mp3'
}

export function audioSource(mediaId: string, format: StreamFormat, offsetMs = 0): string {
  const query = new URLSearchParams({ format })
  if (format !== 'original') query.set('maxBitRate', '192')
  if (format !== 'original' && offsetMs >= 1000) query.set('offsetSeconds', String(Math.floor(offsetMs / 1000)))
  return `/audio/${encodeURIComponent(mediaId)}?${query.toString()}`
}

export function currentOccurrence(session: PlaybackSession | null): string {
  const item = session?.queue[session.queueIndex]
  return item === undefined ? '' : `${item.mediaId}\0${item.addedAt}`
}

export function resumePosition(positionMs: number, durationMs: number): number {
  if (!Number.isFinite(positionMs) || positionMs < 0) return 0
  return durationMs > 0 && positionMs >= durationMs ? 0 : positionMs
}

export function formatTime(positionMs: number): string {
  const seconds = Math.max(0, Math.floor(positionMs / 1000))
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`
}
