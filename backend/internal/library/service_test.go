package library

import (
	"context"
	"errors"
	"testing"

	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
)

type fakeBackend struct {
	albums     func(context.Context, int64, int, int) (navidrome.AlbumPage, error)
	album      func(context.Context, int64, string) (navidrome.AlbumDetail, error)
	artists    func(context.Context, int64) ([]navidrome.Artist, error)
	artist     func(context.Context, int64, string) (navidrome.ArtistDetail, error)
	song       func(context.Context, int64, string) (navidrome.Song, error)
	songs      func(context.Context, int64, int, int) ([]navidrome.Song, error)
	search     func(context.Context, int64, string, int, int, int, int, int, int) (navidrome.SearchResult, error)
	setStar    func(context.Context, int64, string, string, bool) error
	starred    func(context.Context, int64) (navidrome.SearchResult, error)
	scanStatus func(context.Context, int64) (navidrome.ScanStatus, error)
}

func (f *fakeBackend) Albums(ctx context.Context, userID int64, offset, limit int) (navidrome.AlbumPage, error) {
	return f.albums(ctx, userID, offset, limit)
}
func (f *fakeBackend) Album(ctx context.Context, userID int64, id string) (navidrome.AlbumDetail, error) {
	return f.album(ctx, userID, id)
}
func (f *fakeBackend) Artists(ctx context.Context, userID int64) ([]navidrome.Artist, error) {
	return f.artists(ctx, userID)
}
func (f *fakeBackend) Artist(ctx context.Context, userID int64, id string) (navidrome.ArtistDetail, error) {
	return f.artist(ctx, userID, id)
}
func (f *fakeBackend) Song(ctx context.Context, userID int64, id string) (navidrome.Song, error) {
	return f.song(ctx, userID, id)
}
func (f *fakeBackend) Songs(ctx context.Context, userID int64, offset, limit int) ([]navidrome.Song, error) {
	return f.songs(ctx, userID, offset, limit)
}
func (f *fakeBackend) Search(ctx context.Context, userID int64, query string, artistCount, albumCount, songCount, artistOffset, albumOffset, songOffset int) (navidrome.SearchResult, error) {
	return f.search(ctx, userID, query, artistCount, albumCount, songCount, artistOffset, albumOffset, songOffset)
}
func (f *fakeBackend) SetStar(ctx context.Context, userID int64, kind, id string, starred bool) error {
	return f.setStar(ctx, userID, kind, id, starred)
}
func (f *fakeBackend) Starred(ctx context.Context, userID int64) (navidrome.SearchResult, error) {
	return f.starred(ctx, userID)
}
func (f *fakeBackend) ScanStatus(ctx context.Context, userID int64) (navidrome.ScanStatus, error) {
	return f.scanStatus(ctx, userID)
}

func TestAlbumsUseOpaqueCursorAndLookaheadPagination(t *testing.T) {
	backend := &fakeBackend{}
	backend.albums = func(_ context.Context, userID int64, offset, limit int) (navidrome.AlbumPage, error) {
		if userID != 12 || offset != 0 || limit != 3 {
			t.Fatalf("Albums args=(%d,%d,%d)", userID, offset, limit)
		}
		return navidrome.AlbumPage{Items: []navidrome.Album{
			{ID: "a1", Name: "First", Artist: "Artist", CoverArt: "al-a1", Duration: 10},
			{ID: "a2", Name: "Second", Artist: "Artist", Duration: 20},
			{ID: "a3", Name: "Lookahead"},
		}}, nil
	}
	page, err := New(backend).Albums(context.Background(), 12, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor == nil || page.Items[0].CoverURL != "/audio/coverart/al-a1" || page.Items[0].DurationMS != 10_000 {
		t.Fatalf("page = %#v", page)
	}
	if _, err := decodeCursor(*page.NextCursor); err != nil {
		t.Fatalf("next cursor is not valid: %v", err)
	}
}

func TestLibraryHandlesEmptyAndMissingMetadata(t *testing.T) {
	backend := &fakeBackend{}
	backend.albums = func(context.Context, int64, int, int) (navidrome.AlbumPage, error) {
		return navidrome.AlbumPage{Items: []navidrome.Album{}}, nil
	}
	backend.song = func(context.Context, int64, string) (navidrome.Song, error) {
		return navidrome.Song{ID: "song-1", Name: "Filename title", Duration: -1, Track: -2, Year: -3}, nil
	}
	service := New(backend)
	albums, err := service.Albums(context.Background(), 1, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if albums.Items == nil || len(albums.Items) != 0 || albums.NextCursor != nil {
		t.Fatalf("empty albums = %#v", albums)
	}
	item, err := service.Song(context.Background(), 1, "song-1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "Filename title" || item.DurationMS != 0 || item.TrackNumber != 0 || item.Year != 0 {
		t.Fatalf("missing metadata mapping = %#v", item)
	}
}

func TestArtistsAreSortedAndPaginated(t *testing.T) {
	backend := &fakeBackend{}
	backend.artists = func(context.Context, int64) ([]navidrome.Artist, error) {
		return []navidrome.Artist{{ID: "z", Name: "Zulu"}, {ID: "a", Name: "alpha"}, {ID: "b", Name: "Beta"}}, nil
	}
	page, err := New(backend).Artists(context.Background(), 1, "", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Name != "alpha" || page.Items[1].Name != "Beta" || page.NextCursor == nil {
		t.Fatalf("artists = %#v", page)
	}
}

func TestSearchValidationDefaultsAndEmptyResult(t *testing.T) {
	backend := &fakeBackend{}
	backend.search = func(_ context.Context, _ int64, query string, artistCount, albumCount, songCount, _, _, _ int) (navidrome.SearchResult, error) {
		if query != "needle" || artistCount != 7 || albumCount != 7 || songCount != 7 {
			t.Fatalf("Search args query=%q counts=(%d,%d,%d)", query, artistCount, albumCount, songCount)
		}
		return navidrome.SearchResult{Albums: []navidrome.Album{}, Artists: []navidrome.Artist{}, Songs: []navidrome.Song{}}, nil
	}
	result, err := New(backend).Search(context.Background(), 1, " needle ", 7, -1, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Albums == nil || result.Artists == nil || result.Songs == nil {
		t.Fatalf("empty search arrays must be non-nil: %#v", result)
	}
	for _, query := range []string{"", "   "} {
		if _, err := New(backend).Search(context.Background(), 1, query, 10, -1, -1, -1); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("query %q error = %v", query, err)
		}
	}
}

func TestLibraryValidationAndUpstreamErrors(t *testing.T) {
	backend := &fakeBackend{}
	backend.song = func(context.Context, int64, string) (navidrome.Song, error) {
		return navidrome.Song{}, navidrome.ErrUnavailable
	}
	service := New(backend)
	if _, err := service.Song(context.Background(), 1, "../bad"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid id error = %v", err)
	}
	if _, err := service.Song(context.Background(), 1, "song-1"); !errors.Is(err, navidrome.ErrUnavailable) {
		t.Fatalf("upstream error = %v", err)
	}
	if _, err := service.Albums(context.Background(), 1, "not-a-cursor", 20); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cursor error = %v", err)
	}
	if ValidArtworkID("../../etc/passwd") || !ValidArtworkID("al-abc_1a") {
		t.Fatal("artwork identifier validation is unsafe")
	}
}
