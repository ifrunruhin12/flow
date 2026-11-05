package library

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
)

var (
	ErrInvalidInput = errors.New("invalid library input")
	resourceID      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	artworkID       = regexp.MustCompile(`^(?:mf|ar|al|pl|dc|ra)-[A-Za-z0-9][A-Za-z0-9:_-]{0,190}(?:_[0-9a-f]{1,16})?$`)
)

const (
	DefaultLimit = 50
	MaxLimit     = 100
	MaxQuerySize = 200
)

type Backend interface {
	Albums(context.Context, int64, int, int) (navidrome.AlbumPage, error)
	Album(context.Context, int64, string) (navidrome.AlbumDetail, error)
	Artists(context.Context, int64) ([]navidrome.Artist, error)
	Artist(context.Context, int64, string) (navidrome.ArtistDetail, error)
	Song(context.Context, int64, string) (navidrome.Song, error)
	Songs(context.Context, int64, int, int) ([]navidrome.Song, error)
	Search(context.Context, int64, string, int, int, int, int, int, int) (navidrome.SearchResult, error)
	SetStar(context.Context, int64, string, string, bool) error
	Starred(context.Context, int64) (navidrome.SearchResult, error)
	ScanStatus(context.Context, int64) (navidrome.ScanStatus, error)
}

type Service struct {
	backend Backend
}

func New(backend Backend) *Service {
	return &Service{backend: backend}
}

type Album struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	AlbumArtist string `json:"albumArtist"`
	ArtistID    string `json:"artistId,omitempty"`
	CoverURL    string `json:"coverUrl,omitempty"`
	SongCount   int    `json:"songCount"`
	DurationMS  int64  `json:"durationMs"`
	Year        int    `json:"year,omitempty"`
	Starred     bool   `json:"starred"`
}

type Song struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	ArtistID    string `json:"artistId,omitempty"`
	Album       string `json:"album"`
	AlbumID     string `json:"albumId,omitempty"`
	DurationMS  int64  `json:"durationMs"`
	TrackNumber int    `json:"trackNumber,omitempty"`
	DiscNumber  int    `json:"discNumber,omitempty"`
	Year        int    `json:"year,omitempty"`
	CoverURL    string `json:"coverUrl,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Suffix      string `json:"suffix,omitempty"`
	Starred     bool   `json:"starred"`
}

type Artist struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	AlbumCount int    `json:"albumCount"`
	CoverURL   string `json:"coverUrl,omitempty"`
	Starred    bool   `json:"starred"`
}

type AlbumDetail struct {
	Album
	Tracks []Song `json:"tracks"`
}

type ArtistDetail struct {
	Artist
	Albums    []Album `json:"albums"`
	TopTracks []Song  `json:"topTracks"`
}

type AlbumPage struct {
	Items      []Album `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

type ArtistPage struct {
	Items      []Artist `json:"items"`
	NextCursor *string  `json:"nextCursor"`
}

type SongPage struct {
	Items      []Song  `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

type SearchResult struct {
	Albums  []Album  `json:"albums"`
	Artists []Artist `json:"artists"`
	Songs   []Song   `json:"songs"`
}

type ScanStatus struct {
	Scanning    bool       `json:"scanning"`
	Count       int64      `json:"count"`
	FolderCount int64      `json:"folderCount"`
	LastScan    *time.Time `json:"lastScan,omitempty"`
	Error       string     `json:"error,omitempty"`
	ScanType    string     `json:"scanType,omitempty"`
	ElapsedMS   int64      `json:"elapsedMs"`
}

func (s *Service) Albums(ctx context.Context, userID int64, cursor string, limit int) (AlbumPage, error) {
	offset, limit, err := page(cursor, limit)
	if err != nil {
		return AlbumPage{}, err
	}
	upstream, err := s.backend.Albums(ctx, userID, offset, limit+1)
	if err != nil {
		return AlbumPage{}, err
	}
	more := len(upstream.Items) > limit
	if more {
		upstream.Items = upstream.Items[:limit]
	}
	items := make([]Album, len(upstream.Items))
	for index, item := range upstream.Items {
		items[index] = album(item)
	}
	var next *string
	if more {
		encoded := encodeCursor(offset + len(items))
		next = &encoded
	}
	return AlbumPage{Items: items, NextCursor: next}, nil
}

func (s *Service) Album(ctx context.Context, userID int64, id string) (AlbumDetail, error) {
	if !ValidResourceID(id) {
		return AlbumDetail{}, ErrInvalidInput
	}
	item, err := s.backend.Album(ctx, userID, id)
	if err != nil {
		return AlbumDetail{}, err
	}
	return AlbumDetail{Album: album(item.Album), Tracks: songs(item.Songs)}, nil
}

func (s *Service) Artists(ctx context.Context, userID int64, cursor string, limit int, query string) (ArtistPage, error) {
	offset, limit, err := page(cursor, limit)
	if err != nil {
		return ArtistPage{}, err
	}
	query = strings.TrimSpace(query)
	if len([]rune(query)) > MaxQuerySize {
		return ArtistPage{}, ErrInvalidInput
	}
	var upstream []navidrome.Artist
	more := false
	if query != "" {
		result, searchErr := s.backend.Search(ctx, userID, query, limit+1, 0, 0, offset, 0, 0)
		if searchErr != nil {
			return ArtistPage{}, searchErr
		}
		upstream = result.Artists
		if len(upstream) > limit {
			more = true
			upstream = upstream[:limit]
		}
	} else {
		upstream, err = s.backend.Artists(ctx, userID)
		if err != nil {
			return ArtistPage{}, err
		}
		sort.SliceStable(upstream, func(left, right int) bool {
			return strings.ToLower(upstream[left].Name) < strings.ToLower(upstream[right].Name)
		})
		if offset > len(upstream) {
			upstream = []navidrome.Artist{}
		} else {
			end := min(offset+limit, len(upstream))
			more = end < len(upstream)
			upstream = upstream[offset:end]
		}
	}
	items := make([]Artist, len(upstream))
	for index, item := range upstream {
		items[index] = artist(item)
	}
	var next *string
	if more {
		encoded := encodeCursor(offset + len(items))
		next = &encoded
	}
	return ArtistPage{Items: items, NextCursor: next}, nil
}

func (s *Service) Artist(ctx context.Context, userID int64, id string) (ArtistDetail, error) {
	if !ValidResourceID(id) {
		return ArtistDetail{}, ErrInvalidInput
	}
	item, err := s.backend.Artist(ctx, userID, id)
	if err != nil {
		return ArtistDetail{}, err
	}
	albums := make([]Album, len(item.Albums))
	for index, value := range item.Albums {
		albums[index] = album(value)
	}
	return ArtistDetail{Artist: artist(item.Artist), Albums: albums, TopTracks: songs(item.TopTracks)}, nil
}

func (s *Service) Songs(ctx context.Context, userID int64, cursor string, limit int, albumID string) (SongPage, error) {
	offset, limit, err := page(cursor, limit)
	if err != nil {
		return SongPage{}, err
	}
	var upstream []navidrome.Song
	more := false
	if albumID != "" {
		if !ValidResourceID(albumID) {
			return SongPage{}, ErrInvalidInput
		}
		item, detailErr := s.backend.Album(ctx, userID, albumID)
		if detailErr != nil {
			return SongPage{}, detailErr
		}
		if offset > len(item.Songs) {
			upstream = []navidrome.Song{}
		} else {
			end := min(offset+limit, len(item.Songs))
			more = end < len(item.Songs)
			upstream = item.Songs[offset:end]
		}
	} else {
		upstream, err = s.backend.Songs(ctx, userID, offset, limit+1)
		if err != nil {
			return SongPage{}, err
		}
		if len(upstream) > limit {
			more = true
			upstream = upstream[:limit]
		}
	}
	items := songs(upstream)
	var next *string
	if more {
		encoded := encodeCursor(offset + len(items))
		next = &encoded
	}
	return SongPage{Items: items, NextCursor: next}, nil
}

func (s *Service) Song(ctx context.Context, userID int64, id string) (Song, error) {
	if !ValidResourceID(id) {
		return Song{}, ErrInvalidInput
	}
	item, err := s.backend.Song(ctx, userID, id)
	if err != nil {
		return Song{}, err
	}
	return song(item), nil
}

func (s *Service) Search(ctx context.Context, userID int64, query string, limit, albumCount, artistCount, songCount int) (SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > MaxQuerySize {
		return SearchResult{}, ErrInvalidInput
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > MaxLimit {
		return SearchResult{}, ErrInvalidInput
	}
	counts := []*int{&albumCount, &artistCount, &songCount}
	for _, count := range counts {
		if *count == -1 {
			*count = limit
		}
		if *count < 0 || *count > MaxLimit {
			return SearchResult{}, ErrInvalidInput
		}
	}
	result, err := s.backend.Search(ctx, userID, query, artistCount, albumCount, songCount, 0, 0, 0)
	if err != nil {
		return SearchResult{}, err
	}
	return searchResult(result), nil
}

func (s *Service) SetStar(ctx context.Context, userID int64, kind, id string, starred bool) error {
	if (kind != "song" && kind != "album") || !ValidResourceID(id) {
		return ErrInvalidInput
	}
	return s.backend.SetStar(ctx, userID, kind, id, starred)
}

func (s *Service) Starred(ctx context.Context, userID int64, kind string) (SearchResult, error) {
	if kind != "" && kind != "song" && kind != "album" {
		return SearchResult{}, ErrInvalidInput
	}
	result, err := s.backend.Starred(ctx, userID)
	if err != nil {
		return SearchResult{}, err
	}
	mapped := searchResult(result)
	if kind == "song" {
		mapped.Albums = []Album{}
		mapped.Artists = []Artist{}
	}
	if kind == "album" {
		mapped.Artists = []Artist{}
		mapped.Songs = []Song{}
	}
	return mapped, nil
}

func (s *Service) ScanStatus(ctx context.Context, userID int64) (ScanStatus, error) {
	status, err := s.backend.ScanStatus(ctx, userID)
	if err != nil {
		return ScanStatus{}, err
	}
	return ScanStatus{Scanning: status.Scanning, Count: status.Count, FolderCount: status.FolderCount,
		LastScan: status.LastScan, Error: status.Error, ScanType: status.ScanType, ElapsedMS: status.Elapsed.Milliseconds()}, nil
}

func ValidResourceID(id string) bool {
	return resourceID.MatchString(id)
}

func ValidArtworkID(id string) bool {
	return artworkID.MatchString(id)
}

func page(cursor string, limit int) (int, int, error) {
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxLimit {
		return 0, 0, ErrInvalidInput
	}
	offset, err := decodeCursor(cursor)
	if err != nil {
		return 0, 0, err
	}
	return offset, limit, nil
}

func encodeCursor(offset int) string {
	payload, _ := json.Marshal(struct {
		Offset int `json:"o"`
	}{Offset: offset})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(payload) > 64 {
		return 0, ErrInvalidInput
	}
	var value struct {
		Offset int `json:"o"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || value.Offset < 0 || encodeCursor(value.Offset) != cursor {
		return 0, ErrInvalidInput
	}
	return value.Offset, nil
}

func album(item navidrome.Album) Album {
	title := strings.TrimSpace(item.Name)
	artistName := strings.TrimSpace(item.Artist)
	return Album{ID: item.ID, Title: title, Artist: artistName, AlbumArtist: artistName, ArtistID: item.ArtistID,
		CoverURL: coverURL(item.CoverArt), SongCount: max(item.SongCount, 0), DurationMS: secondsToMilliseconds(item.Duration),
		Year: max(item.Year, 0), Starred: item.Starred}
}

func artist(item navidrome.Artist) Artist {
	return Artist{ID: item.ID, Name: strings.TrimSpace(item.Name), AlbumCount: max(item.AlbumCount, 0), CoverURL: coverURL(item.CoverArt), Starred: item.Starred}
}

func song(item navidrome.Song) Song {
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = strings.TrimSpace(item.Name)
	}
	return Song{ID: item.ID, Title: title, Artist: strings.TrimSpace(item.Artist), ArtistID: item.ArtistID,
		Album: strings.TrimSpace(item.Album), AlbumID: item.AlbumID, DurationMS: secondsToMilliseconds(item.Duration),
		TrackNumber: max(item.Track, 0), DiscNumber: max(item.DiscNumber, 0), Year: max(item.Year, 0),
		CoverURL: coverURL(item.CoverArt), ContentType: strings.TrimSpace(item.ContentType), Suffix: strings.TrimSpace(item.Suffix), Starred: item.Starred}
}

func songs(items []navidrome.Song) []Song {
	result := make([]Song, len(items))
	for index, item := range items {
		result[index] = song(item)
	}
	return result
}

func searchResult(result navidrome.SearchResult) SearchResult {
	albums := make([]Album, len(result.Albums))
	for index, item := range result.Albums {
		albums[index] = album(item)
	}
	artists := make([]Artist, len(result.Artists))
	for index, item := range result.Artists {
		artists[index] = artist(item)
	}
	return SearchResult{Albums: albums, Artists: artists, Songs: songs(result.Songs)}
}

func coverURL(id string) string {
	if id == "" {
		return ""
	}
	return "/audio/coverart/" + url.PathEscape(id)
}

func secondsToMilliseconds(seconds int) int64 {
	if seconds <= 0 {
		return 0
	}
	return int64(seconds) * 1000
}

func ParseLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	var value int
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil || fmt.Sprintf("%d", value) != raw || value < 1 || value > MaxLimit {
		return 0, ErrInvalidInput
	}
	return value, nil
}
