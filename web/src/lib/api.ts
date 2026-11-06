import type {
  AdminUser,
  Album,
  AlbumDetail,
  Artist,
  ArtistDetail,
  MembershipRequest,
  Page,
  PlaybackMutation,
  PlaybackSession,
  Playlist,
  ScanStatus,
  SearchResult,
  Song,
  User,
} from './types'

interface ErrorEnvelope {
  error?: {
    code?: string
    message?: string
  }
  currentSession?: PlaybackSession
}

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly currentSession?: PlaybackSession

  constructor(status: number, code: string, message: string, currentSession?: PlaybackSession) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.currentSession = currentSession
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  if (init?.body !== undefined) {
    headers.set('Content-Type', 'application/json')
  }
  const response = await fetch(path, {
    ...init,
    credentials: 'include',
    headers,
  })
  if (!response.ok) {
    let payload: ErrorEnvelope = {}
    try {
      payload = (await response.json()) as ErrorEnvelope
    } catch {
      // A generic error below is safer than exposing an untrusted response body.
    }
    throw new ApiError(response.status, payload.error?.code ?? 'request_failed', payload.error?.message ?? 'The request failed.', payload.currentSession)
  }
  if (response.status === 204) {
    return undefined as T
  }
  return (await response.json()) as T
}

export async function getSession(): Promise<User | null> {
  try {
    const payload = await request<{ user: User }>('/api/auth/me')
    return payload.user
  } catch (error: unknown) {
    if (error instanceof ApiError && error.status === 401) {
      return null
    }
    throw error
  }
}

export async function login(username: string, password: string): Promise<User> {
  const payload = await request<{ user: User }>('/api/auth/login', {
    method: 'POST',
    body: JSON.stringify({ username, password }),
  })
  return payload.user
}

export const logout = (): Promise<void> => request('/api/auth/logout', { method: 'POST' })

export const changePassword = (currentPassword: string, newPassword: string): Promise<void> =>
  request('/api/auth/change-password', { method: 'POST', body: JSON.stringify({ currentPassword, newPassword }) })

export async function registerRequest(displayName: string, email: string, desiredUsername: string): Promise<number> {
  const payload = await request<{ requestId: number }>('/api/auth/register-request', {
    method: 'POST',
    body: JSON.stringify({ displayName, email, desiredUsername }),
  })
  return payload.requestId
}

export async function listRequests(): Promise<MembershipRequest[]> {
  const payload = await request<{ items: MembershipRequest[] }>('/api/admin/requests?status=pending')
  return payload.items
}

export const approveRequest = (id: number, temporaryPassword: string, note: string): Promise<{ user: User }> =>
  request(`/api/admin/requests/${id}/approve`, { method: 'POST', body: JSON.stringify({ temporaryPassword, note }) })

export const rejectRequest = (id: number, note: string): Promise<void> =>
  request(`/api/admin/requests/${id}/reject`, { method: 'POST', body: JSON.stringify({ note }) })

export async function listUsers(): Promise<AdminUser[]> {
  const payload = await request<{ items: AdminUser[] }>('/api/admin/users')
  return payload.items
}

export const createMember = (input: { username: string; displayName: string; email: string; temporaryPassword: string }): Promise<{ user: AdminUser }> =>
  request('/api/admin/users', { method: 'POST', body: JSON.stringify(input) })

export const suspendUser = (id: number): Promise<void> => request(`/api/admin/users/${id}/suspend`, { method: 'POST', body: '{}' })
export const reactivateUser = (id: number): Promise<void> => request(`/api/admin/users/${id}/reactivate`, { method: 'POST' })
export const retryProvisioning = (id: number): Promise<void> => request(`/api/admin/users/${id}/retry-provisioning`, { method: 'POST' })
export const resetPassword = (id: number, temporaryPassword: string): Promise<void> =>
  request(`/api/admin/users/${id}/reset-password`, { method: 'POST', body: JSON.stringify({ temporaryPassword }) })
export const deleteUser = (id: number, hard: boolean): Promise<void> =>
  request(`/api/admin/users/${id}${hard ? '?hard=true' : ''}`, { method: 'DELETE' })

function withQuery(path: string, values: Record<string, string | number | undefined>): string {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(values)) {
    if (value !== undefined && value !== '') {
      query.set(key, String(value))
    }
  }
  const encoded = query.toString()
  return encoded === '' ? path : `${path}?${encoded}`
}

export const listAlbums = (cursor?: string): Promise<Page<Album>> =>
  request(withQuery('/api/library/albums', { cursor, limit: 50 }))

export const getAlbum = (id: string): Promise<AlbumDetail> =>
  request(`/api/library/albums/${encodeURIComponent(id)}`)

export const listArtists = (cursor?: string, query?: string): Promise<Page<Artist>> =>
  request(withQuery('/api/library/artists', { cursor, limit: 50, query }))

export const getArtist = (id: string): Promise<ArtistDetail> =>
  request(`/api/library/artists/${encodeURIComponent(id)}`)

export const listSongs = (cursor?: string, albumId?: string): Promise<Page<Song>> =>
  request(withQuery('/api/library/songs', { cursor, limit: 50, albumId }))

export const getSong = (id: string): Promise<Song> =>
  request(`/api/library/songs/${encodeURIComponent(id)}`)

export const searchLibrary = (query: string): Promise<SearchResult> =>
  request(withQuery('/api/library/search', { q: query, limit: 30 }))

export const listStars = (kind?: 'song' | 'album'): Promise<SearchResult> =>
  request(withQuery('/api/library/stars', { kind }))

export const setStar = (mediaId: string, kind: 'song' | 'album'): Promise<void> =>
  request('/api/library/stars', { method: 'POST', body: JSON.stringify({ mediaId, kind }) })

export const removeStar = (mediaId: string, kind: 'song' | 'album'): Promise<void> =>
  request(`/api/library/stars/${kind}/${encodeURIComponent(mediaId)}`, { method: 'DELETE' })

export const getScanStatus = (): Promise<ScanStatus> => request('/api/library/scanner/status')

export async function listPlaylists(): Promise<Playlist[]> {
  const payload = await request<{ items: Playlist[] }>('/api/playlists')
  return payload.items
}

export const getPlaylist = (id: string): Promise<Playlist> =>
  request(`/api/playlists/${encodeURIComponent(id)}`)

export const createPlaylist = (name: string, comment: string, songIds: string[] = []): Promise<Playlist> =>
  request('/api/playlists', { method: 'POST', body: JSON.stringify({ name, comment, songIds }) })

export const updatePlaylist = (
  id: string,
  update: { name?: string; comment?: string; songIds?: string[]; removeIndices?: number[] },
): Promise<Playlist> => request(`/api/playlists/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify(update) })

export const deletePlaylist = (id: string): Promise<void> =>
  request(`/api/playlists/${encodeURIComponent(id)}`, { method: 'DELETE' })

export const getPlaybackSession = (): Promise<PlaybackSession> => request('/api/playback/session')

export const updatePlaybackSession = (mutation: PlaybackMutation): Promise<PlaybackSession> =>
  request('/api/playback/session', { method: 'PUT', body: JSON.stringify(mutation) })

export const clearPlaybackSession = (ifRevision: number): Promise<PlaybackSession> =>
  request('/api/playback/session', { method: 'DELETE', body: JSON.stringify({ ifRevision }) })

export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : 'The operation could not be completed.'
}
