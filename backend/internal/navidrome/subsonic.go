package navidrome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	subsonicVersion = "1.16.1"
	subsonicClient  = "flow"

	subsonicAuthenticationFailed = 40
	subsonicAuthorizationFailed  = 50
	subsonicDataNotFound         = 70
	maxSubsonicJSONBody          = 8 << 20
	maxSubsonicErrorBody         = 64 << 10
)

var (
	ErrAuthentication = errors.New("navidrome authentication failed")
	ErrAuthorization  = errors.New("navidrome authorization failed")
	ErrNotFound       = errors.New("navidrome resource not found")
	ErrUnavailable    = errors.New("navidrome unavailable")
	ErrMalformed      = errors.New("navidrome returned a malformed response")
	ErrRateLimited    = errors.New("navidrome rate limited the request")
)

type Adapter struct {
	client      *Client
	credentials *CredentialCache
}

func NewAdapter(client *Client, credentials *CredentialCache) *Adapter {
	return &Adapter{client: client, credentials: credentials}
}

type Album struct {
	ID        string
	Name      string
	Artist    string
	ArtistID  string
	CoverArt  string
	SongCount int
	Duration  int
	Year      int
	Starred   bool
}

type Artist struct {
	ID         string
	Name       string
	CoverArt   string
	AlbumCount int
	Starred    bool
}

type Song struct {
	ID          string
	Title       string
	Name        string
	Album       string
	AlbumID     string
	Artist      string
	ArtistID    string
	CoverArt    string
	Duration    int
	Track       int
	DiscNumber  int
	Year        int
	ContentType string
	Suffix      string
	Starred     bool
}

type AlbumPage struct {
	Items []Album
}

type AlbumDetail struct {
	Album
	Songs []Song
}

type ArtistDetail struct {
	Artist
	Albums    []Album
	TopTracks []Song
}

type SearchResult struct {
	Albums  []Album
	Artists []Artist
	Songs   []Song
}

type ScanStatus struct {
	Scanning    bool
	Count       int64
	FolderCount int64
	LastScan    *time.Time
	Error       string
	ScanType    string
	Elapsed     time.Duration
}

type Playlist struct {
	ID        string
	Name      string
	Comment   string
	SongCount int
	Duration  int
	CoverArt  string
	ReadOnly  bool
	Songs     []Song
}

type StreamOptions struct {
	Method        string
	Format        string
	MaxBitRate    int
	OffsetSeconds int
	Range         string
}

type ArtworkOptions struct {
	Method          string
	IfNoneMatch     string
	IfModifiedSince string
}

// BinaryResponse intentionally omits the upstream request and URL so an
// internal Navidrome address cannot accidentally be serialized or copied to a
// Flow client.
type BinaryResponse struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

type RateLimitError struct {
	RetryAfter string
}

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

type subsonicError struct {
	Code int    `json:"code"`
	Text string `json:"message"`
}

type subsonicBase struct {
	Status string         `json:"status"`
	Error  *subsonicError `json:"error,omitempty"`
}

type subsonicEnvelope struct {
	Response json.RawMessage `json:"subsonic-response"`
}

type rawAlbum struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Artist    string     `json:"artist"`
	ArtistID  string     `json:"artistId"`
	CoverArt  string     `json:"coverArt"`
	SongCount int        `json:"songCount"`
	Duration  int        `json:"duration"`
	Year      int        `json:"year"`
	Starred   *time.Time `json:"starred"`
}

type rawArtist struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CoverArt   string     `json:"coverArt"`
	AlbumCount int        `json:"albumCount"`
	Starred    *time.Time `json:"starred"`
	Album      []rawAlbum `json:"album"`
}

type rawSong struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Name        string     `json:"name"`
	Album       string     `json:"album"`
	AlbumID     string     `json:"albumId"`
	Artist      string     `json:"artist"`
	ArtistID    string     `json:"artistId"`
	CoverArt    string     `json:"coverArt"`
	Duration    int        `json:"duration"`
	Track       int        `json:"track"`
	DiscNumber  int        `json:"discNumber"`
	Year        int        `json:"year"`
	ContentType string     `json:"contentType"`
	Suffix      string     `json:"suffix"`
	Starred     *time.Time `json:"starred"`
}

type rawPlaylist struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Comment   string    `json:"comment"`
	SongCount int       `json:"songCount"`
	Duration  int       `json:"duration"`
	CoverArt  string    `json:"coverArt"`
	ReadOnly  bool      `json:"readonly"`
	Entry     []rawSong `json:"entry"`
}

func (a *Adapter) Albums(ctx context.Context, userID int64, offset, limit int) (AlbumPage, error) {
	params := url.Values{"type": {"alphabeticalByName"}, "offset": {strconv.Itoa(offset)}, "size": {strconv.Itoa(limit)}}
	var response struct {
		AlbumList struct {
			Albums []rawAlbum `json:"album"`
		} `json:"albumList2"`
	}
	_, err := a.doJSON(ctx, userID, "getAlbumList2", params, &response)
	if err != nil {
		return AlbumPage{}, err
	}
	if err := requireAlbumIDs(response.AlbumList.Albums); err != nil {
		return AlbumPage{}, err
	}
	return AlbumPage{Items: mapAlbums(response.AlbumList.Albums)}, nil
}

func (a *Adapter) Album(ctx context.Context, userID int64, id string) (AlbumDetail, error) {
	var response struct {
		Album struct {
			rawAlbum
			Songs []rawSong `json:"song"`
		} `json:"album"`
	}
	_, err := a.doJSON(ctx, userID, "getAlbum", url.Values{"id": {id}}, &response)
	if err != nil {
		return AlbumDetail{}, err
	}
	if response.Album.ID == "" {
		return AlbumDetail{}, fmt.Errorf("%w: album omitted id", ErrMalformed)
	}
	if err := requireSongIDs(response.Album.Songs); err != nil {
		return AlbumDetail{}, err
	}
	return AlbumDetail{Album: mapAlbum(response.Album.rawAlbum), Songs: mapSongs(response.Album.Songs)}, nil
}

func (a *Adapter) Artists(ctx context.Context, userID int64) ([]Artist, error) {
	var response struct {
		Artists struct {
			Indexes []struct {
				Artists []rawArtist `json:"artist"`
			} `json:"index"`
		} `json:"artists"`
	}
	_, err := a.doJSON(ctx, userID, "getArtists", nil, &response)
	if errors.Is(err, ErrNotFound) {
		return []Artist{}, nil
	}
	if err != nil {
		return nil, err
	}
	items := make([]Artist, 0)
	for _, index := range response.Artists.Indexes {
		if err := requireArtistIDs(index.Artists); err != nil {
			return nil, err
		}
		for _, artist := range index.Artists {
			items = append(items, mapArtist(artist))
		}
	}
	return items, nil
}

func (a *Adapter) Artist(ctx context.Context, userID int64, id string) (ArtistDetail, error) {
	var response struct {
		Artist rawArtist `json:"artist"`
	}
	_, err := a.doJSON(ctx, userID, "getArtist", url.Values{"id": {id}}, &response)
	if err != nil {
		return ArtistDetail{}, err
	}
	if response.Artist.ID == "" {
		return ArtistDetail{}, fmt.Errorf("%w: artist omitted id", ErrMalformed)
	}
	if err := requireAlbumIDs(response.Artist.Album); err != nil {
		return ArtistDetail{}, err
	}
	result := ArtistDetail{Artist: mapArtist(response.Artist), Albums: mapAlbums(response.Artist.Album), TopTracks: []Song{}}
	var top struct {
		TopSongs struct {
			Songs []rawSong `json:"song"`
		} `json:"topSongs"`
	}
	if _, topErr := a.doJSON(ctx, userID, "getTopSongs", url.Values{"artist": {response.Artist.Name}, "count": {"10"}}, &top); topErr == nil {
		if err := requireSongIDs(top.TopSongs.Songs); err != nil {
			return ArtistDetail{}, err
		}
		result.TopTracks = mapSongs(top.TopSongs.Songs)
	} else if !errors.Is(topErr, ErrNotFound) {
		return ArtistDetail{}, topErr
	}
	return result, nil
}

func (a *Adapter) Song(ctx context.Context, userID int64, id string) (Song, error) {
	var response struct {
		Song rawSong `json:"song"`
	}
	_, err := a.doJSON(ctx, userID, "getSong", url.Values{"id": {id}}, &response)
	if err != nil {
		return Song{}, err
	}
	if response.Song.ID == "" {
		return Song{}, fmt.Errorf("%w: song omitted id", ErrMalformed)
	}
	return mapSong(response.Song), nil
}

func (a *Adapter) Songs(ctx context.Context, userID int64, offset, limit int) ([]Song, error) {
	result, err := a.Search(ctx, userID, `""`, 0, 0, limit, 0, 0, offset)
	return result.Songs, err
}

func (a *Adapter) Search(ctx context.Context, userID int64, query string, artistCount, albumCount, songCount, artistOffset, albumOffset, songOffset int) (SearchResult, error) {
	params := url.Values{
		"query":        {query},
		"artistCount":  {strconv.Itoa(artistCount)},
		"albumCount":   {strconv.Itoa(albumCount)},
		"songCount":    {strconv.Itoa(songCount)},
		"artistOffset": {strconv.Itoa(artistOffset)},
		"albumOffset":  {strconv.Itoa(albumOffset)},
		"songOffset":   {strconv.Itoa(songOffset)},
	}
	var response struct {
		Search struct {
			Artists []rawArtist `json:"artist"`
			Albums  []rawAlbum  `json:"album"`
			Songs   []rawSong   `json:"song"`
		} `json:"searchResult3"`
	}
	_, err := a.doJSON(ctx, userID, "search3", params, &response)
	if err != nil {
		return SearchResult{}, err
	}
	if err := requireArtistIDs(response.Search.Artists); err != nil {
		return SearchResult{}, err
	}
	if err := requireAlbumIDs(response.Search.Albums); err != nil {
		return SearchResult{}, err
	}
	if err := requireSongIDs(response.Search.Songs); err != nil {
		return SearchResult{}, err
	}
	artists := make([]Artist, len(response.Search.Artists))
	for index, artist := range response.Search.Artists {
		artists[index] = mapArtist(artist)
	}
	return SearchResult{Albums: mapAlbums(response.Search.Albums), Artists: artists, Songs: mapSongs(response.Search.Songs)}, nil
}

func (a *Adapter) SetStar(ctx context.Context, userID int64, kind, id string, starred bool) error {
	parameter := "id"
	switch kind {
	case "song":
	case "album":
		parameter = "albumId"
	default:
		return errors.New("unsupported star kind")
	}
	command := "star"
	if !starred {
		command = "unstar"
	}
	var response struct{}
	_, err := a.doJSON(ctx, userID, command, url.Values{parameter: {id}}, &response)
	return err
}

func (a *Adapter) Starred(ctx context.Context, userID int64) (SearchResult, error) {
	var response struct {
		Starred struct {
			Artists []rawArtist `json:"artist"`
			Albums  []rawAlbum  `json:"album"`
			Songs   []rawSong   `json:"song"`
		} `json:"starred2"`
	}
	_, err := a.doJSON(ctx, userID, "getStarred2", nil, &response)
	if err != nil {
		return SearchResult{}, err
	}
	if err := requireArtistIDs(response.Starred.Artists); err != nil {
		return SearchResult{}, err
	}
	if err := requireAlbumIDs(response.Starred.Albums); err != nil {
		return SearchResult{}, err
	}
	if err := requireSongIDs(response.Starred.Songs); err != nil {
		return SearchResult{}, err
	}
	artists := make([]Artist, len(response.Starred.Artists))
	for index, artist := range response.Starred.Artists {
		artists[index] = mapArtist(artist)
	}
	return SearchResult{Albums: mapAlbums(response.Starred.Albums), Artists: artists, Songs: mapSongs(response.Starred.Songs)}, nil
}

func (a *Adapter) ScanStatus(ctx context.Context, userID int64) (ScanStatus, error) {
	var response struct {
		Status struct {
			Scanning    bool       `json:"scanning"`
			Count       int64      `json:"count"`
			FolderCount int64      `json:"folderCount"`
			LastScan    *time.Time `json:"lastScan"`
			Error       string     `json:"error"`
			ScanType    string     `json:"scanType"`
			ElapsedTime int64      `json:"elapsedTime"`
		} `json:"scanStatus"`
	}
	_, err := a.doJSON(ctx, userID, "getScanStatus", nil, &response)
	if err != nil {
		return ScanStatus{}, err
	}
	return ScanStatus{
		Scanning: response.Status.Scanning, Count: response.Status.Count, FolderCount: response.Status.FolderCount,
		LastScan: response.Status.LastScan, Error: response.Status.Error, ScanType: response.Status.ScanType,
		Elapsed: time.Duration(response.Status.ElapsedTime),
	}, nil
}

func (a *Adapter) Playlists(ctx context.Context, userID int64) ([]Playlist, error) {
	var response struct {
		Playlists struct {
			Items []rawPlaylist `json:"playlist"`
		} `json:"playlists"`
	}
	_, err := a.doJSON(ctx, userID, "getPlaylists", nil, &response)
	if err != nil {
		return nil, err
	}
	if err := requirePlaylistIDs(response.Playlists.Items); err != nil {
		return nil, err
	}
	return mapPlaylists(response.Playlists.Items), nil
}

func (a *Adapter) Playlist(ctx context.Context, userID int64, id string) (Playlist, error) {
	var response struct {
		Playlist rawPlaylist `json:"playlist"`
	}
	_, err := a.doJSON(ctx, userID, "getPlaylist", url.Values{"id": {id}}, &response)
	if err != nil {
		return Playlist{}, err
	}
	if response.Playlist.ID == "" {
		return Playlist{}, fmt.Errorf("%w: playlist omitted id", ErrMalformed)
	}
	if err := requireSongIDs(response.Playlist.Entry); err != nil {
		return Playlist{}, err
	}
	return mapPlaylist(response.Playlist), nil
}

func (a *Adapter) CreatePlaylist(ctx context.Context, userID int64, name, comment string, songIDs []string) (Playlist, error) {
	params := url.Values{"name": {name}}
	for _, id := range songIDs {
		params.Add("songId", id)
	}
	var response struct {
		Playlist rawPlaylist `json:"playlist"`
	}
	_, err := a.doJSON(ctx, userID, "createPlaylist", params, &response)
	if err != nil {
		return Playlist{}, err
	}
	if response.Playlist.ID == "" {
		return Playlist{}, fmt.Errorf("%w: created playlist omitted id", ErrMalformed)
	}
	// Flow playlists are visible read-only to all members. Navidrome defaults a
	// new playlist to private, so make the visibility explicit.
	if err := a.UpdatePlaylistMetadata(ctx, userID, response.Playlist.ID, nil, &comment, true); err != nil {
		return Playlist{}, err
	}
	return a.Playlist(ctx, userID, response.Playlist.ID)
}

func (a *Adapter) ReplacePlaylistSongs(ctx context.Context, userID int64, id string, songIDs []string) error {
	params := url.Values{"playlistId": {id}}
	for _, songID := range songIDs {
		params.Add("songId", songID)
	}
	var response struct{}
	_, err := a.doJSON(ctx, userID, "createPlaylist", params, &response)
	return err
}

func (a *Adapter) UpdatePlaylistMetadata(ctx context.Context, userID int64, id string, name, comment *string, public bool) error {
	params := url.Values{"playlistId": {id}, "public": {strconv.FormatBool(public)}}
	if name != nil {
		params.Set("name", *name)
	}
	if comment != nil {
		params.Set("comment", *comment)
	}
	var response struct{}
	_, err := a.doJSON(ctx, userID, "updatePlaylist", params, &response)
	return err
}

func (a *Adapter) RemovePlaylistIndices(ctx context.Context, userID int64, id string, indices []int) error {
	params := url.Values{"playlistId": {id}, "public": {"true"}}
	for _, index := range indices {
		params.Add("songIndexToRemove", strconv.Itoa(index))
	}
	var response struct{}
	_, err := a.doJSON(ctx, userID, "updatePlaylist", params, &response)
	return err
}

func (a *Adapter) DeletePlaylist(ctx context.Context, userID int64, id string) error {
	var response struct{}
	_, err := a.doJSON(ctx, userID, "deletePlaylist", url.Values{"id": {id}}, &response)
	return err
}

func (a *Adapter) OpenStream(ctx context.Context, userID int64, mediaID string, options StreamOptions) (BinaryResponse, error) {
	params := url.Values{"id": {mediaID}}
	if options.Format != "" {
		params.Set("format", options.Format)
	}
	if options.MaxBitRate > 0 {
		params.Set("maxBitRate", strconv.Itoa(options.MaxBitRate))
	}
	if options.OffsetSeconds > 0 {
		params.Set("timeOffset", strconv.Itoa(options.OffsetSeconds))
	}
	return a.openBinary(ctx, userID, "stream", params, func(request *http.Request) {
		if options.Method != "" {
			request.Method = options.Method
		}
		if options.Range != "" {
			request.Header.Set("Range", options.Range)
		}
	}, "audio/")
}

func (a *Adapter) OpenArtwork(ctx context.Context, userID int64, artworkID string, options ArtworkOptions) (BinaryResponse, error) {
	return a.openBinary(ctx, userID, "getCoverArt", url.Values{"id": {artworkID}}, func(request *http.Request) {
		if options.Method != "" {
			request.Method = options.Method
		}
		if options.IfNoneMatch != "" {
			request.Header.Set("If-None-Match", options.IfNoneMatch)
		}
		if options.IfModifiedSince != "" {
			request.Header.Set("If-Modified-Since", options.IfModifiedSince)
		}
	}, "image/")
}

func (a *Adapter) doJSON(ctx context.Context, userID int64, command string, params url.Values, target any) (http.Header, error) {
	credential, err := a.credentials.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	header, err := a.client.subsonicJSON(ctx, command, params, credential, target)
	if !errors.Is(err, ErrAuthentication) {
		return header, err
	}
	a.credentials.InvalidateIfMatch(userID, credential.Token)
	credential, refreshErr := a.credentials.Get(ctx, userID)
	if refreshErr != nil {
		return nil, refreshErr
	}
	return a.client.subsonicJSON(ctx, command, params, credential, target)
}

func (a *Adapter) openBinary(ctx context.Context, userID int64, command string, params url.Values, configure func(*http.Request), contentPrefix string) (BinaryResponse, error) {
	credential, err := a.credentials.Get(ctx, userID)
	if err != nil {
		return BinaryResponse{}, err
	}
	response, err := a.client.subsonicBinary(ctx, command, params, credential, configure, contentPrefix)
	if !errors.Is(err, ErrAuthentication) {
		return response, err
	}
	a.credentials.InvalidateIfMatch(userID, credential.Token)
	credential, refreshErr := a.credentials.Get(ctx, userID)
	if refreshErr != nil {
		return BinaryResponse{}, refreshErr
	}
	return a.client.subsonicBinary(ctx, command, params, credential, configure, contentPrefix)
}

func (c *Client) subsonicJSON(ctx context.Context, command string, params url.Values, credential TokenCredential, target any) (http.Header, error) {
	request, err := c.subsonicRequest(ctx, command, params, credential)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, classifyTransportError(command, err)
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body, maxSubsonicJSONBody)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return response.Header.Clone(), &RateLimitError{RetryAfter: response.Header.Get("Retry-After")}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return response.Header.Clone(), ErrAuthentication
	}
	if response.StatusCode != http.StatusOK {
		if response.StatusCode >= 500 {
			return response.Header.Clone(), ErrUnavailable
		}
		return response.Header.Clone(), &HTTPError{Operation: command, StatusCode: response.StatusCode}
	}
	root, err := parseSubsonic(body)
	if err != nil {
		return response.Header.Clone(), err
	}
	if err := json.Unmarshal(root, target); err != nil {
		return response.Header.Clone(), fmt.Errorf("%w: decode %s", ErrMalformed, command)
	}
	return response.Header.Clone(), nil
}

func (c *Client) subsonicBinary(ctx context.Context, command string, params url.Values, credential TokenCredential, configure func(*http.Request), contentPrefix string) (BinaryResponse, error) {
	request, err := c.subsonicRequest(ctx, command, params, credential)
	if err != nil {
		return BinaryResponse{}, err
	}
	if configure != nil {
		configure(request)
	}
	response, err := c.streamHTTP.Do(request)
	if err != nil {
		return BinaryResponse{}, classifyTransportError(command, err)
	}
	if response.StatusCode == http.StatusTooManyRequests {
		response.Body.Close()
		return BinaryResponse{}, &RateLimitError{RetryAfter: response.Header.Get("Retry-After")}
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		response.Body.Close()
		return BinaryResponse{}, ErrAuthentication
	}
	contentType := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Type")))
	if strings.HasPrefix(contentType, "application/json") {
		body, readErr := readLimited(response.Body, maxSubsonicErrorBody)
		response.Body.Close()
		if readErr != nil {
			return BinaryResponse{}, readErr
		}
		_, parseErr := parseSubsonic(body)
		if parseErr != nil {
			return BinaryResponse{}, parseErr
		}
		return BinaryResponse{}, fmt.Errorf("%w: unexpected success envelope", ErrMalformed)
	}
	if response.StatusCode >= 500 {
		response.Body.Close()
		return BinaryResponse{}, ErrUnavailable
	}
	if response.StatusCode == http.StatusNotModified {
		response.Body.Close()
		header := response.Header.Clone()
		header.Del("Content-Length")
		return BinaryResponse{StatusCode: response.StatusCode, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	if response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		response.Body.Close()
		header := response.Header.Clone()
		header.Del("Content-Length")
		header.Del("Content-Type")
		return BinaryResponse{StatusCode: response.StatusCode, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return BinaryResponse{}, &HTTPError{Operation: command, StatusCode: response.StatusCode}
	}
	if !strings.HasPrefix(contentType, contentPrefix) {
		response.Body.Close()
		return BinaryResponse{}, fmt.Errorf("%w: unexpected %s content type", ErrMalformed, command)
	}
	return BinaryResponse{StatusCode: response.StatusCode, Header: response.Header.Clone(), Body: response.Body}, nil
}

func (c *Client) subsonicRequest(ctx context.Context, command string, params url.Values, credential TokenCredential) (*http.Request, error) {
	if command == "" || strings.ContainsAny(command, "/?#") {
		return nil, errors.New("invalid Navidrome Subsonic command")
	}
	values := make(url.Values, len(params)+5)
	for key, items := range params {
		for _, item := range items {
			values.Add(key, item)
		}
	}
	values.Set("u", credential.Username)
	values.Set("jwt", credential.Token)
	values.Set("v", subsonicVersion)
	values.Set("c", subsonicClient)
	values.Set("f", "json")
	request, err := c.request(ctx, http.MethodGet, "rest/"+command, nil)
	if err != nil {
		return nil, err
	}
	request.URL.RawQuery = values.Encode()
	return request, nil
}

func parseSubsonic(body []byte) (json.RawMessage, error) {
	var envelope subsonicEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Response) == 0 {
		return nil, ErrMalformed
	}
	var base subsonicBase
	if err := json.Unmarshal(envelope.Response, &base); err != nil {
		return nil, ErrMalformed
	}
	switch base.Status {
	case "ok":
		return envelope.Response, nil
	case "failed":
		if base.Error == nil {
			return nil, ErrMalformed
		}
		switch base.Error.Code {
		case subsonicAuthenticationFailed:
			return nil, ErrAuthentication
		case subsonicAuthorizationFailed:
			return nil, ErrAuthorization
		case subsonicDataNotFound:
			return nil, ErrNotFound
		default:
			return nil, &HTTPError{Operation: "subsonic", StatusCode: http.StatusBadGateway}
		}
	default:
		return nil, ErrMalformed
	}
}

func readLimited(reader io.Reader, maximum int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, ErrUnavailable
	}
	if int64(len(body)) > maximum {
		return nil, ErrMalformed
	}
	return body, nil
}

func classifyTransportError(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	_ = operation
	return ErrUnavailable
}

func mapAlbum(item rawAlbum) Album {
	return Album{ID: item.ID, Name: item.Name, Artist: item.Artist, ArtistID: item.ArtistID, CoverArt: item.CoverArt,
		SongCount: item.SongCount, Duration: item.Duration, Year: item.Year, Starred: item.Starred != nil}
}

func requireAlbumIDs(items []rawAlbum) error {
	for _, item := range items {
		if item.ID == "" {
			return fmt.Errorf("%w: album omitted id", ErrMalformed)
		}
	}
	return nil
}

func requireArtistIDs(items []rawArtist) error {
	for _, item := range items {
		if item.ID == "" {
			return fmt.Errorf("%w: artist omitted id", ErrMalformed)
		}
	}
	return nil
}

func requireSongIDs(items []rawSong) error {
	for _, item := range items {
		if item.ID == "" {
			return fmt.Errorf("%w: song omitted id", ErrMalformed)
		}
	}
	return nil
}

func requirePlaylistIDs(items []rawPlaylist) error {
	for _, item := range items {
		if item.ID == "" {
			return fmt.Errorf("%w: playlist omitted id", ErrMalformed)
		}
	}
	return nil
}

func mapAlbums(items []rawAlbum) []Album {
	result := make([]Album, len(items))
	for index, item := range items {
		result[index] = mapAlbum(item)
	}
	return result
}

func mapArtist(item rawArtist) Artist {
	return Artist{ID: item.ID, Name: item.Name, CoverArt: item.CoverArt, AlbumCount: item.AlbumCount, Starred: item.Starred != nil}
}

func mapSong(item rawSong) Song {
	return Song{ID: item.ID, Title: item.Title, Name: item.Name, Album: item.Album, AlbumID: item.AlbumID,
		Artist: item.Artist, ArtistID: item.ArtistID, CoverArt: item.CoverArt, Duration: item.Duration,
		Track: item.Track, DiscNumber: item.DiscNumber, Year: item.Year, ContentType: item.ContentType,
		Suffix: item.Suffix, Starred: item.Starred != nil}
}

func mapSongs(items []rawSong) []Song {
	result := make([]Song, len(items))
	for index, item := range items {
		result[index] = mapSong(item)
	}
	return result
}

func mapPlaylist(item rawPlaylist) Playlist {
	return Playlist{ID: item.ID, Name: item.Name, Comment: item.Comment, SongCount: item.SongCount, Duration: item.Duration,
		CoverArt: item.CoverArt, ReadOnly: item.ReadOnly, Songs: mapSongs(item.Entry)}
}

func mapPlaylists(items []rawPlaylist) []Playlist {
	result := make([]Playlist, len(items))
	for index, item := range items {
		result[index] = mapPlaylist(item)
	}
	return result
}
