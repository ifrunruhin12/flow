import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from 'react'
import {
  createPlaylist,
  deletePlaylist,
  errorMessage,
  getAlbum,
  getArtist,
  getPlaylist,
  getScanStatus,
  listAlbums,
  listArtists,
  listPlaylists,
  listSongs,
  removeStar,
  searchLibrary,
  setStar,
  updatePlaylist,
} from '../../lib/api'
import type { Album, Artist, Playlist, Song } from '../../lib/types'
import { usePlayer } from '../player'

type Section = 'albums' | 'artists' | 'songs' | 'search' | 'playlists'

export function LibraryExperience({
  isAdmin,
}: {
  isAdmin: boolean
}) {
  const { controller } = usePlayer()
  const onPlay = (songs: Song[], index: number) => { void controller.startQueue(songs, index) }
  const [section, setSection] = useState<Section>('albums')
  const scan = useQuery({ queryKey: ['library', 'scan'], queryFn: getScanStatus, refetchInterval: 30_000 })

  return (
    <main className="pb-44">
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <nav className="flex flex-wrap gap-2" aria-label="Music library">
          {(['albums', 'artists', 'songs', 'search', 'playlists'] as const).map((item) => (
            <button key={item} className={section === item ? 'button-primary' : 'button-secondary'} onClick={() => setSection(item)}>
              {item[0]?.toUpperCase()}{item.slice(1)}
            </button>
          ))}
        </nav>
        <p className="text-xs text-[var(--flow-muted)]">
          {scan.data?.scanning === true ? `Scanning · ${scan.data.count} items` : scan.isError ? 'Scan status unavailable' : 'Library ready'}
        </p>
      </div>
      {section === 'albums' && <AlbumsView onPlay={onPlay} />}
      {section === 'artists' && <ArtistsView onPlay={onPlay} />}
      {section === 'songs' && <SongsView onPlay={onPlay} />}
      {section === 'search' && <SearchView onPlay={onPlay} />}
      {section === 'playlists' && <PlaylistsView isAdmin={isAdmin} onPlay={onPlay} />}
    </main>
  )
}

function AlbumsView({ onPlay }: { onPlay: (songs: Song[], index: number) => void }) {
  const { pending, loading } = usePlayer()
  const [selected, setSelected] = useState('')
  const albums = useInfiniteQuery({
    queryKey: ['library', 'albums'],
    initialPageParam: '',
    queryFn: ({ pageParam }) => listAlbums(pageParam === '' ? undefined : pageParam),
    getNextPageParam: (page) => page.nextCursor ?? undefined,
  })
  const items = albums.data?.pages.flatMap((page) => page.items) ?? []
  const detail = useQuery({ queryKey: ['library', 'album', selected], queryFn: () => getAlbum(selected), enabled: selected !== '' })
  return (
    <LibraryPanel title="Albums" detail="Choose an album and make time for the music." error={albums.error}>
      {selected !== '' && (
        <DetailPanel onClose={() => setSelected('')}>
          {detail.isPending && <Loading />}
          {detail.isError && <ErrorText error={detail.error} />}
          {detail.data !== undefined && (
            <>
              <div className="mb-5 flex flex-wrap items-start gap-5">
                <Artwork src={detail.data.coverUrl} alt="" size="large" />
                <div className="min-w-0 flex-1">
                  <p className="text-xs font-bold uppercase tracking-widest text-[var(--flow-accent-strong)]">Album</p>
                  <h3 className="mt-2 text-3xl font-black">{detail.data.title || 'Untitled album'}</h3>
                  <p className="mt-2 text-[var(--flow-muted)]">{detail.data.albumArtist || detail.data.artist || 'Unknown artist'}</p>
                  <div className="mt-4 flex gap-2">
                    <button className="button-primary" disabled={pending || loading || detail.data.tracks.length === 0} onClick={() => onPlay(detail.data.tracks, 0)}>Play album</button>
                    <StarButton id={detail.data.id} kind="album" starred={detail.data.starred} />
                  </div>
                </div>
              </div>
              <SongList songs={detail.data.tracks} onPlay={onPlay} />
            </>
          )}
        </DetailPanel>
      )}
      <div className="album-grid">
        {items.map((album) => <AlbumCard key={album.id} album={album} onOpen={() => setSelected(album.id)} />)}
      </div>
      <CollectionFooter empty={items.length === 0} pending={albums.isPending} error={albums.error} hasMore={albums.hasNextPage} loadingMore={albums.isFetchingNextPage} loadMore={() => void albums.fetchNextPage()} />
    </LibraryPanel>
  )
}

function ArtistsView({ onPlay }: { onPlay: (songs: Song[], index: number) => void }) {
  const [selected, setSelected] = useState('')
  const [selectedAlbum, setSelectedAlbum] = useState('')
  const artists = useInfiniteQuery({
    queryKey: ['library', 'artists'],
    initialPageParam: '',
    queryFn: ({ pageParam }) => listArtists(pageParam === '' ? undefined : pageParam),
    getNextPageParam: (page) => page.nextCursor ?? undefined,
  })
  const items = artists.data?.pages.flatMap((page) => page.items) ?? []
  const detail = useQuery({ queryKey: ['library', 'artist', selected], queryFn: () => getArtist(selected), enabled: selected !== '' })
  return (
    <LibraryPanel title="Artists" detail="Discographies and top tracks from the member-scoped library." error={artists.error}>
      {selected !== '' && (
        <DetailPanel onClose={() => setSelected('')}>
          {detail.isPending && <Loading />}
          {detail.isError && <ErrorText error={detail.error} />}
          {detail.data !== undefined && (
            <>
              <div className="mb-6 flex items-center gap-5">
                <Artwork src={detail.data.coverUrl} alt="" size="large" />
                <div><p className="text-xs font-bold uppercase tracking-widest text-[var(--flow-accent-strong)]">Artist</p><h3 className="mt-2 text-3xl font-black">{detail.data.name || 'Unknown artist'}</h3></div>
              </div>
              {detail.data.topTracks.length > 0 && <><h4 className="mb-3 text-lg font-black">Top tracks</h4><SongList songs={detail.data.topTracks} onPlay={onPlay} /></>}
              <h4 className="mb-3 mt-7 text-lg font-black">Discography</h4>
              <div className="album-grid">{detail.data.albums.map((album) => <AlbumCard key={album.id} album={album} onOpen={() => setSelectedAlbum(album.id)} />)}</div>
            </>
          )}
        </DetailPanel>
      )}
      {selectedAlbum !== '' && <AlbumPlaybackDetails id={selectedAlbum} onClose={() => setSelectedAlbum('')} onPlay={onPlay} />}
      <div className="album-grid">
        {items.map((artist) => <ArtistCard key={artist.id} artist={artist} onOpen={() => setSelected(artist.id)} />)}
      </div>
      <CollectionFooter empty={items.length === 0} pending={artists.isPending} error={artists.error} hasMore={artists.hasNextPage} loadingMore={artists.isFetchingNextPage} loadMore={() => void artists.fetchNextPage()} />
    </LibraryPanel>
  )
}

function SongsView({ onPlay }: { onPlay: (songs: Song[], index: number) => void }) {
  const songs = useInfiniteQuery({
    queryKey: ['library', 'songs'],
    initialPageParam: '',
    queryFn: ({ pageParam }) => listSongs(pageParam === '' ? undefined : pageParam),
    getNextPageParam: (page) => page.nextCursor ?? undefined,
  })
  const items = songs.data?.pages.flatMap((page) => page.items) ?? []
  return (
    <LibraryPanel title="Songs" detail="Find a track, press play, or add it to your queue." error={songs.error}>
      <SongList songs={items} onPlay={(songs, index) => { const song = songs[index]; if (song !== undefined) onPlay([song], 0) }} />
      <CollectionFooter empty={items.length === 0} pending={songs.isPending} error={songs.error} hasMore={songs.hasNextPage} loadingMore={songs.isFetchingNextPage} loadMore={() => void songs.fetchNextPage()} />
    </LibraryPanel>
  )
}

function SearchView({ onPlay }: { onPlay: (songs: Song[], index: number) => void }) {
  const [input, setInput] = useState('')
  const [query, setQuery] = useState('')
  const [selectedAlbum, setSelectedAlbum] = useState('')
  const [selectedArtist, setSelectedArtist] = useState('')
  const search = useQuery({ queryKey: ['library', 'search', query], queryFn: () => searchLibrary(query), enabled: query !== '' })
  const submit = (event: FormEvent) => {
    event.preventDefault()
    setQuery(input.trim())
  }
  return (
    <LibraryPanel title="Search" detail="Search songs, albums, and artists through Flow." error={search.error}>
      <form className="mb-7 flex gap-2" onSubmit={submit}>
        <input className="field" maxLength={200} placeholder="Search the library" value={input} onChange={(event) => setInput(event.target.value)} />
        <button className="button-primary" disabled={input.trim() === ''}>Search</button>
      </form>
      {search.isPending && query !== '' && <Loading />}
      {search.data !== undefined && (
        <div className="space-y-8">
          <SearchGroup title="Songs" empty={search.data.songs.length === 0}><SongList songs={search.data.songs} onPlay={(songs, index) => { const song = songs[index]; if (song !== undefined) onPlay([song], 0) }} /></SearchGroup>
          <SearchGroup title="Albums" empty={search.data.albums.length === 0}><div className="album-grid">{search.data.albums.map((album) => <AlbumCard key={album.id} album={album} onOpen={() => setSelectedAlbum(album.id)} />)}</div></SearchGroup>
          <SearchGroup title="Artists" empty={search.data.artists.length === 0}><div className="album-grid">{search.data.artists.map((artist) => <ArtistCard key={artist.id} artist={artist} onOpen={() => setSelectedArtist(artist.id)} />)}</div></SearchGroup>
        </div>
      )}
      {selectedArtist !== '' && <ArtistPlaybackDetails id={selectedArtist} onClose={() => setSelectedArtist('')} onPlay={onPlay} />}
      {selectedAlbum !== '' && <AlbumPlaybackDetails id={selectedAlbum} onClose={() => setSelectedAlbum('')} onPlay={onPlay} />}
      {query === '' && <EmptyText>Enter a title, album, or artist.</EmptyText>}
    </LibraryPanel>
  )
}

function PlaylistsView({ isAdmin, onPlay }: { isAdmin: boolean; onPlay: (songs: Song[], index: number) => void }) {
  const queryClient = useQueryClient()
  const [selected, setSelected] = useState('')
  const [name, setName] = useState('')
  const [comment, setComment] = useState('')
  const playlists = useQuery({ queryKey: ['library', 'playlists'], queryFn: listPlaylists })
  const detail = useQuery({ queryKey: ['library', 'playlist', selected], queryFn: () => getPlaylist(selected), enabled: selected !== '' })
  const create = useMutation({
    mutationFn: () => createPlaylist(name, comment),
    onSuccess: (item) => {
      setName('')
      setComment('')
      setSelected(item.id)
      return queryClient.invalidateQueries({ queryKey: ['library', 'playlists'] })
    },
  })
  return (
    <LibraryPanel title="Playlists" detail={isAdmin ? 'Public playlists are curated by the Flow administrator.' : 'Administrator-curated playlists are read-only for members.'} error={playlists.error}>
      {isAdmin && (
        <form className="panel mb-6 grid gap-3 p-5 sm:grid-cols-[1fr_1fr_auto]" onSubmit={(event) => { event.preventDefault(); create.mutate() }}>
          <input className="field" required maxLength={100} placeholder="Playlist name" value={name} onChange={(event) => setName(event.target.value)} />
          <input className="field" maxLength={1000} placeholder="Comment (optional)" value={comment} onChange={(event) => setComment(event.target.value)} />
          <button className="button-primary" disabled={create.isPending}>Create</button>
          {create.isError && <div className="sm:col-span-3"><ErrorText error={create.error} /></div>}
        </form>
      )}
      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
        {playlists.data?.map((item) => (
          <button key={item.id} className="panel flex items-center gap-4 p-4 text-left" onClick={() => setSelected(item.id)}>
            <Artwork src={item.coverUrl} alt="" />
            <span><strong className="block">{item.name || 'Untitled playlist'}</strong><span className="text-sm text-[var(--flow-muted)]">{item.songCount} tracks</span></span>
          </button>
        ))}
      </div>
      {playlists.isPending && <Loading />}
      {playlists.data?.length === 0 && <EmptyText>No playlists yet.</EmptyText>}
      {selected !== '' && <PlaylistDetail item={detail.data} pending={detail.isPending} error={detail.error} isAdmin={isAdmin} onClose={() => setSelected('')} onPlay={onPlay} />}
    </LibraryPanel>
  )
}

function PlaylistDetail({ item, pending, error, isAdmin, onClose, onPlay }: { item?: Playlist; pending: boolean; error: Error | null; isAdmin: boolean; onClose: () => void; onPlay: (songs: Song[], index: number) => void }) {
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [comment, setComment] = useState('')
  const [songIDs, setSongIDs] = useState<string[]>([])
  const [addID, setAddID] = useState('')
  useEffect(() => {
    if (item !== undefined) {
      setName(item.name)
      setComment(item.comment ?? '')
      setSongIDs((item.tracks ?? []).map((song) => song.id))
    }
  }, [item])
  const save = useMutation({
    mutationFn: () => item === undefined ? Promise.reject(new Error('Playlist is unavailable.')) : updatePlaylist(item.id, { name, comment, songIds: songIDs }),
    onSuccess: () => Promise.all([
      queryClient.invalidateQueries({ queryKey: ['library', 'playlists'] }),
      queryClient.invalidateQueries({ queryKey: ['library', 'playlist', item?.id] }),
    ]),
  })
  const remove = useMutation({
    mutationFn: () => item === undefined ? Promise.reject(new Error('Playlist is unavailable.')) : deletePlaylist(item.id),
    onSuccess: () => {
      onClose()
      return queryClient.invalidateQueries({ queryKey: ['library', 'playlists'] })
    },
  })
  const tracksByID = useMemo(() => new Map((item?.tracks ?? []).map((song) => [song.id, song])), [item])
  const orderedTracks = songIDs.map((id) => tracksByID.get(id)).filter((song): song is Song => song !== undefined)
  const move = (from: number, to: number) => {
    if (to < 0 || to >= songIDs.length) return
    const next = [...songIDs]
    const [moved] = next.splice(from, 1)
    if (moved === undefined) return
    next.splice(to, 0, moved)
    setSongIDs(next)
  }
  return (
    <DetailPanel onClose={onClose}>
      {pending && <Loading />}
      {error !== null && <ErrorText error={error} />}
      {item !== undefined && (
        <>
          {isAdmin ? (
            <div className="mb-5 grid gap-3 sm:grid-cols-2">
              <input className="field" value={name} maxLength={100} onChange={(event) => setName(event.target.value)} />
              <input className="field" value={comment} maxLength={1000} placeholder="Comment" onChange={(event) => setComment(event.target.value)} />
              <div className="flex gap-2 sm:col-span-2"><input className="field" placeholder="Add a song by media ID" value={addID} onChange={(event) => setAddID(event.target.value)} /><button className="button-secondary" onClick={() => { const id = addID.trim(); if (id !== '') { setSongIDs([...songIDs, id]); setAddID('') } }}>Add</button></div>
              <div className="flex flex-wrap gap-2 sm:col-span-2"><button className="button-primary" disabled={save.isPending || name.trim() === ''} onClick={() => save.mutate()}>Save playlist</button><button className="button-danger" disabled={remove.isPending} onClick={() => { if (window.confirm(`Delete ${item.name}?`)) remove.mutate() }}>Delete</button></div>
              {(save.isError || remove.isError) && <div className="sm:col-span-2"><ErrorText error={save.error ?? remove.error} /></div>}
            </div>
          ) : <><h3 className="text-2xl font-black">{item.name}</h3><p className="mb-5 text-[var(--flow-muted)]">{item.comment}</p></>}
          {isAdmin && songIDs.map((id, index) => {
            const song = tracksByID.get(id)
            return <div key={`${id}-${index}`} className="song-row"><span className="min-w-0 flex-1"><strong className="block truncate">{song?.title ?? id}</strong><span className="block truncate text-sm text-[var(--flow-muted)]">{song?.artist ?? 'Pending lookup after save'}</span></span><button className="player-control" disabled={index === 0} onClick={() => move(index, index - 1)}>↑</button><button className="player-control" disabled={index + 1 === songIDs.length} onClick={() => move(index, index + 1)}>↓</button><button className="player-control" onClick={() => setSongIDs(songIDs.filter((_, itemIndex) => itemIndex !== index))}>×</button></div>
          })}
          {!isAdmin && <SongList songs={item.tracks ?? []} onPlay={onPlay} />}
          {isAdmin && orderedTracks.length > 0 && <button className="button-secondary mt-4" onClick={() => onPlay(orderedTracks, 0)}>Preview playlist</button>}
        </>
      )}
    </DetailPanel>
  )
}

function SongList({ songs, onPlay }: { songs: Song[]; onPlay: (songs: Song[], index: number) => void }) {
  const { controller, session, pending, loading } = usePlayer()
  if (songs.length === 0) return <EmptyText>No tracks found.</EmptyText>
  return <div className="divide-y divide-[var(--flow-border)]">{songs.map((song, index) => (
    <div key={`${song.id}-${index}`} className="song-row">
      <button className="player-control player-control-primary" disabled={pending || loading} onClick={() => onPlay(songs, index)} aria-label={`Play ${song.title}`}>▶</button>
      <span className="min-w-0 flex-1"><strong className="block truncate">{song.title || 'Untitled track'}{session?.currentMediaId === song.id && <span className="ml-2 text-xs text-[var(--flow-accent-strong)]">Current</span>}</strong><span className="block truncate text-sm text-[var(--flow-muted)]">{[song.artist, song.album].filter(Boolean).join(' · ') || 'Unknown metadata'}</span></span>
      <span className="text-xs text-[var(--flow-muted)]">{formatDuration(song.durationMs)}</span>
      <button className="player-control" disabled={pending || loading} onClick={() => { void controller.add(song.id) }} aria-label={`Add ${song.title} to queue`}>+</button>
      <StarButton id={song.id} kind="song" starred={song.starred} compact />
    </div>
  ))}</div>
}

function AlbumPlaybackDetails({ id, onClose, onPlay }: { id: string; onClose: () => void; onPlay: (songs: Song[], index: number) => void }) {
  const { pending, loading } = usePlayer()
  const album = useQuery({ queryKey: ['library', 'album', id], queryFn: () => getAlbum(id) })
  return <DetailPanel onClose={onClose}>
    {album.isPending && <Loading />}
    {album.isError && <ErrorText error={album.error} />}
    {album.data !== undefined && <>
      <div className="mb-5 flex items-center gap-5"><Artwork src={album.data.coverUrl} alt="" size="large" /><div><h3 className="text-2xl font-black">{album.data.title}</h3><p className="mt-2 text-[var(--flow-muted)]">{album.data.artist}</p><button className="button-primary mt-3" disabled={pending || loading || album.data.tracks.length === 0} onClick={() => { if (album.data !== undefined) onPlay(album.data.tracks, 0) }}>Play album</button></div></div>
      <SongList songs={album.data.tracks} onPlay={onPlay} />
    </>}
  </DetailPanel>
}

function ArtistPlaybackDetails({ id, onClose, onPlay }: { id: string; onClose: () => void; onPlay: (songs: Song[], index: number) => void }) {
  const [albumId, setAlbumId] = useState('')
  const artist = useQuery({ queryKey: ['library', 'artist', id], queryFn: () => getArtist(id) })
  return <>
    <DetailPanel onClose={onClose}>
      {artist.isPending && <Loading />}
      {artist.isError && <ErrorText error={artist.error} />}
      {artist.data !== undefined && <><h3 className="mb-4 text-2xl font-black">{artist.data.name}</h3><SongList songs={artist.data.topTracks} onPlay={onPlay} /><div className="album-grid mt-6">{artist.data.albums.map((album) => <AlbumCard key={album.id} album={album} onOpen={() => setAlbumId(album.id)} />)}</div></>}
    </DetailPanel>
    {albumId !== '' && <AlbumPlaybackDetails id={albumId} onClose={() => setAlbumId('')} onPlay={onPlay} />}
  </>
}

function AlbumCard({ album, onOpen }: { album: Album; onOpen?: () => void }) {
  return <article className="album-card"><button className="block w-full text-left" onClick={onOpen}><Artwork src={album.coverUrl} alt="" size="card" /><strong className="mt-3 block truncate">{album.title || 'Untitled album'}</strong><span className="block truncate text-sm text-[var(--flow-muted)]">{album.albumArtist || album.artist || 'Unknown artist'}</span></button><div className="mt-3 flex items-center justify-between"><span className="text-xs text-[var(--flow-muted)]">{album.songCount} tracks</span><StarButton id={album.id} kind="album" starred={album.starred} compact /></div></article>
}

function ArtistCard({ artist, onOpen }: { artist: Artist; onOpen?: () => void }) {
  return <article className="album-card"><button className="block w-full text-left" onClick={onOpen}><Artwork src={artist.coverUrl} alt="" size="card" /><strong className="mt-3 block truncate">{artist.name || 'Unknown artist'}</strong><span className="text-sm text-[var(--flow-muted)]">{artist.albumCount} albums</span></button></article>
}

function StarButton({ id, kind, starred, compact = false }: { id: string; kind: 'song' | 'album'; starred: boolean; compact?: boolean }) {
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: () => starred ? removeStar(id, kind) : setStar(id, kind),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['library'] }),
  })
  return <button className={compact ? 'star-button' : 'button-secondary'} disabled={mutation.isPending} onClick={(event) => { event.stopPropagation(); mutation.mutate() }} aria-label={starred ? 'Remove favorite' : 'Add favorite'}>{starred ? '★' : '☆'}</button>
}

function Artwork({ src, alt, size = 'default' }: { src?: string; alt: string; size?: 'default' | 'large' | 'card' }) {
  const classes = size === 'large' ? 'artwork artwork-large' : size === 'card' ? 'artwork artwork-card' : 'artwork'
  return src === undefined || src === '' ? <div className={`${classes} grid place-items-center text-2xl text-[var(--flow-muted)]`}>♫</div> : <img className={classes} src={src} alt={alt} loading="lazy" />
}

function LibraryPanel({ title, detail, error, children }: { title: string; detail: string; error: Error | null; children: ReactNode }) {
  return <section><div className="mb-6"><h2 className="text-3xl font-black">{title}</h2><p className="mt-1 text-sm text-[var(--flow-muted)]">{detail}</p>{error !== null && <ErrorText error={error} />}</div>{children}</section>
}

function DetailPanel({ onClose, children }: { onClose: () => void; children: ReactNode }) {
  return <section className="panel relative mb-7 p-6"><button className="absolute right-4 top-4 player-control" onClick={onClose} aria-label="Close details">×</button>{children}</section>
}

function CollectionFooter({ empty, pending, error, hasMore, loadingMore, loadMore }: { empty: boolean; pending: boolean; error: Error | null; hasMore: boolean; loadingMore: boolean; loadMore: () => void }) {
  if (pending) return <Loading />
  if (error !== null) return null
  if (empty) return <EmptyText>The library is empty.</EmptyText>
  return hasMore ? <div className="mt-6 text-center"><button className="button-secondary" disabled={loadingMore} onClick={loadMore}>{loadingMore ? 'Loading…' : 'Load more'}</button></div> : null
}

function SearchGroup({ title, empty, children }: { title: string; empty: boolean; children: ReactNode }) {
  return <section><h3 className="mb-3 text-xl font-black">{title}</h3>{empty ? <EmptyText>No matches.</EmptyText> : children}</section>
}

function Loading() { return <p className="py-5 text-[var(--flow-muted)]">Loading…</p> }
function EmptyText({ children }: { children: ReactNode }) { return <p className="panel mt-4 p-6 text-[var(--flow-muted)]">{children}</p> }
function ErrorText({ error }: { error: unknown }) { return <p className="mt-3 text-sm text-[var(--flow-danger)]">{errorMessage(error)}</p> }

function formatDuration(milliseconds: number): string {
  const total = Math.max(0, Math.floor(milliseconds / 1000))
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`
}
