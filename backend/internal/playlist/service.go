package playlist

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
)

var ErrInvalidInput = errors.New("invalid playlist input")

type Backend interface {
	Playlists(context.Context, int64) ([]navidrome.Playlist, error)
	Playlist(context.Context, int64, string) (navidrome.Playlist, error)
	CreatePlaylist(context.Context, int64, string, string, []string) (navidrome.Playlist, error)
	ReplacePlaylistSongs(context.Context, int64, string, []string) error
	UpdatePlaylistMetadata(context.Context, int64, string, *string, *string, bool) error
	RemovePlaylistIndices(context.Context, int64, string, []int) error
	DeletePlaylist(context.Context, int64, string) error
}

type Service struct {
	backend Backend
}

func New(backend Backend) *Service {
	return &Service{backend: backend}
}

type Item struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Comment    string         `json:"comment,omitempty"`
	SongCount  int            `json:"songCount"`
	DurationMS int64          `json:"durationMs"`
	CoverURL   string         `json:"coverUrl,omitempty"`
	Tracks     []library.Song `json:"tracks,omitempty"`
}

type Update struct {
	Name          *string
	Comment       *string
	SongIDs       *[]string
	RemoveIndices []int
}

func (s *Service) List(ctx context.Context, userID int64) ([]Item, error) {
	items, err := s.backend.Playlists(ctx, userID)
	if err != nil {
		return nil, err
	}
	result := make([]Item, len(items))
	for index, item := range items {
		result[index] = mapItem(item, false)
	}
	return result, nil
}

func (s *Service) Get(ctx context.Context, userID int64, id string) (Item, error) {
	if !library.ValidResourceID(id) {
		return Item{}, ErrInvalidInput
	}
	item, err := s.backend.Playlist(ctx, userID, id)
	if err != nil {
		return Item{}, err
	}
	return mapItem(item, true), nil
}

func (s *Service) Create(ctx context.Context, userID int64, name, comment string, songIDs []string) (Item, error) {
	name = strings.TrimSpace(name)
	comment = strings.TrimSpace(comment)
	if len([]rune(name)) < 1 || len([]rune(name)) > 100 || len([]rune(comment)) > 1000 || !validSongIDs(songIDs) {
		return Item{}, ErrInvalidInput
	}
	item, err := s.backend.CreatePlaylist(ctx, userID, name, comment, songIDs)
	if err != nil {
		return Item{}, err
	}
	return mapItem(item, true), nil
}

func (s *Service) Update(ctx context.Context, userID int64, id string, update Update) (Item, error) {
	if !library.ValidResourceID(id) || (update.SongIDs != nil && len(update.RemoveIndices) > 0) {
		return Item{}, ErrInvalidInput
	}
	if update.Name != nil {
		trimmed := strings.TrimSpace(*update.Name)
		if len([]rune(trimmed)) < 1 || len([]rune(trimmed)) > 100 {
			return Item{}, ErrInvalidInput
		}
		*update.Name = trimmed
	}
	if update.Comment != nil {
		trimmed := strings.TrimSpace(*update.Comment)
		if len([]rune(trimmed)) > 1000 {
			return Item{}, ErrInvalidInput
		}
		*update.Comment = trimmed
	}
	if update.SongIDs != nil {
		if !validSongIDs(*update.SongIDs) {
			return Item{}, ErrInvalidInput
		}
		if err := s.backend.ReplacePlaylistSongs(ctx, userID, id, *update.SongIDs); err != nil {
			return Item{}, err
		}
	} else if len(update.RemoveIndices) > 0 {
		seen := make(map[int]struct{}, len(update.RemoveIndices))
		for _, index := range update.RemoveIndices {
			if index < 0 {
				return Item{}, ErrInvalidInput
			}
			if _, duplicate := seen[index]; duplicate {
				return Item{}, ErrInvalidInput
			}
			seen[index] = struct{}{}
		}
		if err := s.backend.RemovePlaylistIndices(ctx, userID, id, update.RemoveIndices); err != nil {
			return Item{}, err
		}
	}
	if update.Name != nil || update.Comment != nil {
		if err := s.backend.UpdatePlaylistMetadata(ctx, userID, id, update.Name, update.Comment, true); err != nil {
			return Item{}, err
		}
	}
	return s.Get(ctx, userID, id)
}

func (s *Service) Delete(ctx context.Context, userID int64, id string) error {
	if !library.ValidResourceID(id) {
		return ErrInvalidInput
	}
	return s.backend.DeletePlaylist(ctx, userID, id)
}

func validSongIDs(ids []string) bool {
	if len(ids) > 5000 {
		return false
	}
	for _, id := range ids {
		if !library.ValidResourceID(id) {
			return false
		}
	}
	return true
}

func mapItem(item navidrome.Playlist, tracks bool) Item {
	result := Item{ID: item.ID, Name: strings.TrimSpace(item.Name), Comment: strings.TrimSpace(item.Comment),
		SongCount: max(item.SongCount, 0), DurationMS: int64(max(item.Duration, 0)) * 1000}
	if item.CoverArt != "" {
		result.CoverURL = "/audio/coverart/" + url.PathEscape(item.CoverArt)
	}
	if tracks {
		result.Tracks = make([]library.Song, len(item.Songs))
		for index, song := range item.Songs {
			title := strings.TrimSpace(song.Title)
			if title == "" {
				title = strings.TrimSpace(song.Name)
			}
			mapped := library.Song{ID: song.ID, Title: title, Artist: strings.TrimSpace(song.Artist), ArtistID: song.ArtistID,
				Album: strings.TrimSpace(song.Album), AlbumID: song.AlbumID, DurationMS: int64(max(song.Duration, 0)) * 1000,
				TrackNumber: max(song.Track, 0), DiscNumber: max(song.DiscNumber, 0), Year: max(song.Year, 0),
				ContentType: strings.TrimSpace(song.ContentType), Suffix: strings.TrimSpace(song.Suffix), Starred: song.Starred}
			if song.CoverArt != "" {
				mapped.CoverURL = "/audio/coverart/" + url.PathEscape(song.CoverArt)
			}
			result.Tracks[index] = mapped
		}
	}
	return result
}
