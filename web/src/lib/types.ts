export type Role = 'admin' | 'member'
export type UserStatus = 'active' | 'suspended' | 'deleted'
export type ProvisioningState =
  | 'not_started'
  | 'credential_ready'
  | 'creating_nd_user'
  | 'linking'
  | 'complete'
  | 'failed'

export interface User {
  id: number
  username: string
  displayName: string
  email?: string
  role: Role
  status: UserStatus
  provisioningState: ProvisioningState
  provisioningFailedStep?: string
  mustChangePassword: boolean
  createdAt?: string
  lastActiveAt?: string
}

export interface AdminUser extends User {
  navidromeCleanupState: 'not_requested' | 'pending' | 'complete' | 'failed'
}

export interface MembershipRequest {
  id: number
  displayName: string
  email: string
  desiredUsername: string
  status: 'pending' | 'approved' | 'rejected'
  adminNote?: string
  rejectNote?: string
  createdAt: string
}

export interface Album {
  id: string
  title: string
  artist: string
  albumArtist: string
  artistId?: string
  coverUrl?: string
  songCount: number
  durationMs: number
  year?: number
  starred: boolean
}

export interface Song {
  id: string
  title: string
  artist: string
  artistId?: string
  album: string
  albumId?: string
  durationMs: number
  trackNumber?: number
  discNumber?: number
  year?: number
  coverUrl?: string
  contentType?: string
  suffix?: string
  starred: boolean
}

export interface Artist {
  id: string
  name: string
  albumCount: number
  coverUrl?: string
  starred: boolean
}

export interface AlbumDetail extends Album {
  tracks: Song[]
}

export interface ArtistDetail extends Artist {
  albums: Album[]
  topTracks: Song[]
}

export interface Page<T> {
  items: T[]
  nextCursor: string | null
}

export interface SearchResult {
  albums: Album[]
  artists: Artist[]
  songs: Song[]
}

export interface Playlist {
  id: string
  name: string
  comment?: string
  songCount: number
  durationMs: number
  coverUrl?: string
  tracks?: Song[]
}

export interface ScanStatus {
  scanning: boolean
  count: number
  folderCount: number
  lastScan?: string
  error?: string
  scanType?: string
  elapsedMs: number
}

export interface PlaybackQueueItem {
  mediaId: string
  addedAt: string
}

export interface PlaybackSession {
  queue: PlaybackQueueItem[]
  queueIndex: number
  currentMediaId: string | null
  positionMs: number
  volume: number
  shuffle: boolean
  status: 'paused' | 'playing'
  revision: number
  updatedAt: string
}

export interface PlaybackMutation {
  ifRevision: number
  queue?: { mediaId: string; addedAt?: string }[]
  queueIndex?: number
  currentMediaId?: string | null
  positionMs?: number
  volume?: number
  shuffle?: boolean
  status?: 'paused' | 'playing'
  command?: 'next' | 'previous' | 'ended' | 'checkpoint'
  queueEdit?: { add?: string[]; remove?: number[]; move?: { from: number; to: number } }
}
