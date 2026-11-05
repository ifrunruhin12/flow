package playlist

import (
	"context"
	"errors"
	"testing"

	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
)

type fakeBackend struct {
	createdUser int64
	replaced    []string
	metadata    bool
	removed     []int
}

func (*fakeBackend) Playlists(context.Context, int64) ([]navidrome.Playlist, error) {
	return []navidrome.Playlist{}, nil
}
func (*fakeBackend) Playlist(context.Context, int64, string) (navidrome.Playlist, error) {
	return navidrome.Playlist{ID: "playlist-1", Name: "List", Songs: []navidrome.Song{}}, nil
}
func (f *fakeBackend) CreatePlaylist(_ context.Context, userID int64, name, comment string, songIDs []string) (navidrome.Playlist, error) {
	f.createdUser = userID
	return navidrome.Playlist{ID: "playlist-1", Name: name, Comment: comment, SongCount: len(songIDs), Songs: []navidrome.Song{}}, nil
}
func (f *fakeBackend) ReplacePlaylistSongs(_ context.Context, _ int64, _ string, songIDs []string) error {
	f.replaced = append([]string(nil), songIDs...)
	return nil
}
func (f *fakeBackend) UpdatePlaylistMetadata(context.Context, int64, string, *string, *string, bool) error {
	f.metadata = true
	return nil
}
func (f *fakeBackend) RemovePlaylistIndices(_ context.Context, _ int64, _ string, indices []int) error {
	f.removed = append([]int(nil), indices...)
	return nil
}
func (*fakeBackend) DeletePlaylist(context.Context, int64, string) error { return nil }

func TestCreateUsesAuthenticatedAdminIdentityAndMapsTracks(t *testing.T) {
	backend := &fakeBackend{}
	item, err := New(backend).Create(context.Background(), 99, "  Road trip  ", " note ", []string{"song-1"})
	if err != nil {
		t.Fatal(err)
	}
	if backend.createdUser != 99 || item.ID != "playlist-1" || item.Name != "Road trip" || item.Comment != "note" || item.Tracks == nil {
		t.Fatalf("item=%#v backend=%#v", item, backend)
	}
}

func TestUpdateValidatesMutuallyExclusiveOperations(t *testing.T) {
	backend := &fakeBackend{}
	songs := []string{"song-1"}
	if _, err := New(backend).Update(context.Background(), 1, "playlist-1", Update{SongIDs: &songs, RemoveIndices: []int{0}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("mixed update error = %v", err)
	}
	if _, err := New(backend).Update(context.Background(), 1, "playlist-1", Update{RemoveIndices: []int{1, 1}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate removal error = %v", err)
	}
	if _, err := New(backend).Update(context.Background(), 1, "../playlist", Update{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsafe id error = %v", err)
	}
}

func TestUpdateReplacesSongsAndKeepsPlaylistPublic(t *testing.T) {
	backend := &fakeBackend{}
	name := "Updated"
	songs := []string{"song-2", "song-1"}
	if _, err := New(backend).Update(context.Background(), 7, "playlist-1", Update{Name: &name, SongIDs: &songs}); err != nil {
		t.Fatal(err)
	}
	if len(backend.replaced) != 2 || backend.replaced[0] != "song-2" || !backend.metadata {
		t.Fatalf("backend = %#v", backend)
	}
}
