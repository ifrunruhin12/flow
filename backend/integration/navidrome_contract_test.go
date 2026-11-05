//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	flownd "github.com/ifrunruhin12/flow/backend/internal/navidrome"
)

const ndAuthorizationHeader = "X-ND-Authorization"

type realNavidrome struct {
	baseURL  string
	username string
	password string
	token    string
	http     *http.Client
}

type staticCredentialSource struct {
	username string
	password string
}

func (s staticCredentialSource) LoadNavidromeCredential(context.Context, int64) (flownd.PasswordCredential, error) {
	return flownd.PasswordCredential{Username: s.username, Password: s.password}, nil
}

func TestPinnedNavidromeContract(t *testing.T) {
	baseURL := strings.TrimRight(os.Getenv("FLOW_REAL_NAVIDROME_URL"), "/")
	username := os.Getenv("FLOW_REAL_NAVIDROME_ADMIN_USERNAME")
	password := os.Getenv("FLOW_REAL_NAVIDROME_ADMIN_PASSWORD")
	if baseURL == "" || username == "" || password == "" {
		t.Skip("real Navidrome environment is not configured")
	}
	if os.Getenv("FLOW_REAL_NAVIDROME_ALLOW_MUTATION") != "true" {
		t.Skip("real Navidrome contract suite requires an explicitly disposable instance")
	}

	real := &realNavidrome{baseURL: baseURL, username: username, password: password, http: &http.Client{Timeout: 20 * time.Second}}
	if os.Getenv("FLOW_REAL_NAVIDROME_CREATE_ADMIN") == "true" {
		status := real.createAdmin(t)
		if status != http.StatusOK {
			t.Fatalf("createAdmin status = %d", status)
		}
		if status := real.createAdmin(t); status != http.StatusForbidden {
			t.Fatalf("second createAdmin status = %d, want 403", status)
		}
	}

	client, err := flownd.New(baseURL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := client.VerifyAdmin(context.Background(), username, password)
	if err != nil {
		t.Fatalf("VerifyAdmin() error = %v", err)
	}
	t.Logf("verified admin=%s id=%s", verification.Username, verification.UserID)
	real.login(t)
	real.verifyServerVersion(t, "0.64.2")

	libraries := real.nativeLibraries(t)
	memberName := "flow_contract_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	memberPassword := "flow-contract-password-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	memberID, err := client.EnsureMemberIdentity(context.Background(), flownd.AdminCredential{Username: username, Password: password}, flownd.MemberInput{
		Username: memberName, Name: memberName, Email: memberName + "@example.invalid", Password: memberPassword,
	})
	if err != nil {
		t.Fatalf("EnsureMemberIdentity() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.DeleteMember(context.Background(), flownd.AdminCredential{Username: username, Password: password}, memberID); err != nil {
			t.Errorf("DeleteMember() cleanup error = %v", err)
		}
	})
	real.updateAndReadMember(t, memberID, memberName)
	if len(libraries) == 0 {
		t.Fatal("pinned Navidrome contract has no library to assign")
	}
	if err := client.AssignAllLibraries(context.Background(), flownd.AdminCredential{Username: username, Password: password}, memberID); err != nil {
		t.Fatalf("AssignAllLibraries() error = %v", err)
	}
	real.verifyAssignedLibraries(t, memberID, libraries)
	resolvedID, exists, err := client.ResolveMemberIdentity(context.Background(), flownd.AdminCredential{Username: username, Password: password}, memberName, memberPassword)
	if err != nil || !exists || resolvedID != memberID {
		t.Fatalf("ResolveMemberIdentity() id=%q exists=%t error=%v", resolvedID, exists, err)
	}

	member := &realNavidrome{baseURL: baseURL, username: memberName, password: memberPassword, http: real.http}
	member.login(t)
	member.subsonicOK(t, "ping", nil)
	member.verifyMemberRoles(t)
	member.subsonicFailure(t, "getUsers", nil)
	member.verifyNativeUserAdminBoundary(t)
	member.subsonicOK(t, "getAlbumList2", url.Values{"type": {"newest"}, "size": {"10"}})
	member.subsonicOK(t, "search3", url.Values{"query": {"flow"}, "songCount": {"10"}})
	member.subsonicOK(t, "getPlaylists", nil)
	member.verifyDirectPlaylistWriteSurface(t)
	member.subsonicFailure(t, "startScan", nil)
	member.subsonicHTTPStatus(t, "getShares", nil, http.StatusNotImplemented)

	real.subsonicOK(t, "startScan", nil)
	real.waitForScan(t)
	mediaID, coverArtID := member.firstSong(t)
	if mediaID == "" {
		t.Fatal("pinned Navidrome contract library has no song fixture")
	}
	member.verifyOriginalRange(t, mediaID)
	member.verifyTranscode(t, mediaID, "mp3")
	member.verifyTranscode(t, mediaID, "opus")
	member.subsonicFailure(t, "download", url.Values{"id": {mediaID}})
	verifyM2Adapter(t, client, username, password, memberName, memberPassword, mediaID, coverArtID)
}

func verifyM2Adapter(t *testing.T, client *flownd.Client, adminUsername, adminPassword, memberUsername, memberPassword, mediaID, coverArtID string) {
	t.Helper()
	memberCache := flownd.NewCredentialCache(staticCredentialSource{username: memberUsername, password: memberPassword}, client)
	memberAdapter := flownd.NewAdapter(client, memberCache)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	albums, err := memberAdapter.Albums(ctx, 101, 0, 10)
	if err != nil {
		t.Fatalf("M2 adapter Albums() error = %v", err)
	}
	if len(albums.Items) == 0 {
		t.Fatal("M2 adapter Albums() returned no fixture album")
	}
	album, err := memberAdapter.Album(ctx, 101, albums.Items[0].ID)
	if err != nil {
		t.Fatalf("M2 adapter Album() error = %v", err)
	}
	if album.ID == "" || len(album.Songs) == 0 {
		t.Fatalf("M2 adapter Album() result = %#v", album)
	}
	artists, err := memberAdapter.Artists(ctx, 101)
	if err != nil {
		t.Fatalf("M2 adapter Artists() error = %v", err)
	}
	if len(artists) == 0 {
		t.Fatal("M2 adapter Artists() returned no fixture artist")
	}
	artist, err := memberAdapter.Artist(ctx, 101, artists[0].ID)
	if err != nil {
		t.Fatalf("M2 adapter Artist() error = %v", err)
	}
	if artist.ID == "" || len(artist.Albums) == 0 {
		t.Fatalf("M2 adapter Artist() result = %#v", artist)
	}
	flatSongs, err := memberAdapter.Songs(ctx, 101, 0, 10)
	if err != nil {
		t.Fatalf("M2 adapter Songs() error = %v", err)
	}
	if !containsSong(flatSongs, mediaID) {
		t.Fatalf("M2 adapter Songs() omitted fixture media %q: %#v", mediaID, flatSongs)
	}
	if _, err := memberAdapter.Search(ctx, 101, "flow", 10, 10, 10, 0, 0, 0); err != nil {
		t.Fatalf("M2 adapter Search() error = %v", err)
	}
	if _, err := memberAdapter.Song(ctx, 101, mediaID); err != nil {
		t.Fatalf("M2 adapter Song() error = %v", err)
	}
	if _, err := memberAdapter.ScanStatus(ctx, 101); err != nil {
		t.Fatalf("M2 adapter ScanStatus() error = %v", err)
	}

	original, err := memberAdapter.OpenStream(ctx, 101, mediaID, flownd.StreamOptions{Format: "raw", Range: "bytes=0-31"})
	if err != nil {
		t.Fatalf("M2 adapter original OpenStream() error = %v", err)
	}
	_, _ = io.Copy(io.Discard, original.Body)
	_ = original.Body.Close()
	if original.StatusCode != http.StatusPartialContent || original.Header.Get("Content-Range") == "" || !strings.EqualFold(original.Header.Get("Accept-Ranges"), "bytes") {
		t.Fatalf("M2 adapter original status=%d headers=%v", original.StatusCode, original.Header)
	}

	transcoded, err := memberAdapter.OpenStream(ctx, 101, mediaID, flownd.StreamOptions{Format: "mp3", MaxBitRate: 64, OffsetSeconds: 1, Range: "bytes=0-31"})
	if err != nil {
		t.Fatalf("M2 adapter transcoded OpenStream() error = %v", err)
	}
	_, _ = io.CopyN(io.Discard, transcoded.Body, 64<<10)
	_ = transcoded.Body.Close()
	if transcoded.StatusCode != http.StatusOK || transcoded.Header.Get("Accept-Ranges") != "none" {
		t.Fatalf("M2 adapter transcode status=%d headers=%v", transcoded.StatusCode, transcoded.Header)
	}
	opus, err := memberAdapter.OpenStream(ctx, 101, mediaID, flownd.StreamOptions{Format: "opus", MaxBitRate: 64, OffsetSeconds: 1})
	if err != nil {
		t.Fatalf("M2 adapter Opus OpenStream() error = %v", err)
	}
	_, _ = io.CopyN(io.Discard, opus.Body, 64<<10)
	_ = opus.Body.Close()
	if opus.StatusCode != http.StatusOK || opus.Header.Get("Accept-Ranges") != "none" {
		t.Fatalf("M2 adapter Opus status=%d headers=%v", opus.StatusCode, opus.Header)
	}

	if coverArtID != "" {
		artwork, err := memberAdapter.OpenArtwork(ctx, 101, coverArtID, flownd.ArtworkOptions{})
		if err != nil {
			t.Fatalf("M2 adapter OpenArtwork() error = %v", err)
		}
		_, _ = io.Copy(io.Discard, artwork.Body)
		_ = artwork.Body.Close()
		if artwork.StatusCode != http.StatusOK || !strings.HasPrefix(artwork.Header.Get("Content-Type"), "image/") {
			t.Fatalf("M2 adapter artwork status=%d content-type=%q", artwork.StatusCode, artwork.Header.Get("Content-Type"))
		}
	}

	if err := memberAdapter.SetStar(ctx, 101, "song", mediaID, true); err != nil {
		t.Fatalf("M2 adapter SetStar(true) error = %v", err)
	}
	starred, err := memberAdapter.Starred(ctx, 101)
	if err != nil {
		t.Fatalf("M2 adapter Starred() error = %v", err)
	}
	if !containsSong(starred.Songs, mediaID) {
		t.Fatalf("M2 adapter Starred() omitted media %q: %#v", mediaID, starred.Songs)
	}
	if err := memberAdapter.SetStar(ctx, 101, "song", mediaID, false); err != nil {
		t.Fatalf("M2 adapter SetStar(false) error = %v", err)
	}

	adminCache := flownd.NewCredentialCache(staticCredentialSource{username: adminUsername, password: adminPassword}, client)
	adminAdapter := flownd.NewAdapter(client, adminCache)
	created, err := adminAdapter.CreatePlaylist(ctx, 1, "flow-m2-contract", "Flow M2 public playlist", []string{mediaID})
	if err != nil {
		t.Fatalf("M2 adapter CreatePlaylist() error = %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if err := adminAdapter.DeletePlaylist(cleanupCtx, 1, created.ID); err != nil {
			t.Errorf("M2 adapter playlist cleanup error = %v", err)
		}
	})
	updatedName := "flow-m2-contract-updated"
	updatedComment := "Flow M2 updated public playlist"
	if err := adminAdapter.UpdatePlaylistMetadata(ctx, 1, created.ID, &updatedName, &updatedComment, true); err != nil {
		t.Fatalf("M2 adapter UpdatePlaylistMetadata() error = %v", err)
	}
	if err := adminAdapter.ReplacePlaylistSongs(ctx, 1, created.ID, []string{mediaID}); err != nil {
		t.Fatalf("M2 adapter ReplacePlaylistSongs() error = %v", err)
	}
	playlists, err := memberAdapter.Playlists(ctx, 101)
	if err != nil {
		t.Fatalf("M2 adapter member Playlists() error = %v", err)
	}
	found := false
	for _, item := range playlists {
		if item.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("admin-created public playlist was not visible to the member adapter")
	}
	memberPlaylist, err := memberAdapter.Playlist(ctx, 101, created.ID)
	if err != nil {
		t.Fatalf("M2 adapter member Playlist() error = %v", err)
	}
	if memberPlaylist.Name != updatedName || memberPlaylist.Comment != updatedComment || !containsSong(memberPlaylist.Songs, mediaID) {
		t.Fatalf("M2 adapter member Playlist() omitted media %q: %#v", mediaID, memberPlaylist.Songs)
	}
}

func containsSong(items []flownd.Song, mediaID string) bool {
	for _, item := range items {
		if item.ID == mediaID {
			return true
		}
	}
	return false
}

func (n *realNavidrome) createAdmin(t *testing.T) int {
	t.Helper()
	response := n.requestJSON(t, http.MethodPost, "/auth/createAdmin", map[string]any{"username": n.username, "password": n.password}, "")
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func (n *realNavidrome) login(t *testing.T) {
	t.Helper()
	response := n.requestJSON(t, http.MethodPost, "/auth/login", map[string]any{"username": n.username, "password": n.password}, "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", response.StatusCode)
	}
	var payload struct {
		Token   string `json:"token"`
		IsAdmin bool   `json:"isAdmin"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Token == "" {
		t.Fatal("login response omitted token")
	}
	n.token = payload.Token
}

func (n *realNavidrome) verifyServerVersion(t *testing.T, expected string) {
	t.Helper()
	root := n.subsonicOK(t, "ping", nil)
	serverVersion, _ := root["serverVersion"].(string)
	if serverVersion != expected && !strings.HasPrefix(serverVersion, expected+" (") {
		t.Fatalf("Navidrome serverVersion = %q, want release %s", serverVersion, expected)
	}
}

func (n *realNavidrome) nativeLibraries(t *testing.T) []int {
	t.Helper()
	response := n.requestJSON(t, http.MethodGet, "/api/library/", nil, n.token)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list libraries status = %d", response.StatusCode)
	}
	var libraries []struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&libraries); err != nil {
		t.Fatal(err)
	}
	ids := make([]int, len(libraries))
	for i := range libraries {
		ids[i] = libraries[i].ID
	}
	return ids
}

func (n *realNavidrome) createMember(t *testing.T, username, password string) string {
	t.Helper()
	response := n.requestJSON(t, http.MethodPost, "/api/user/", map[string]any{
		"userName": username,
		"name":     username,
		"email":    username + "@example.invalid",
		"isAdmin":  false,
		"password": password,
	}, n.token)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("create member status = %d body=%q", response.StatusCode, body)
	}
	_, _ = io.Copy(io.Discard, response.Body)

	response = n.requestJSON(t, http.MethodGet, "/api/user/", nil, n.token)
	defer response.Body.Close()
	var users []struct {
		ID       string `json:"id"`
		Username string `json:"userName"`
	}
	if err := json.NewDecoder(response.Body).Decode(&users); err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		if user.Username == username {
			return user.ID
		}
	}
	t.Fatal("created member was not returned by native user API")
	return ""
}

func (n *realNavidrome) updateAndReadMember(t *testing.T, userID, username string) {
	t.Helper()
	path := "/api/user/" + url.PathEscape(userID) + "/"
	response := n.requestJSON(t, http.MethodGet, path, nil, n.token)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("read member status = %d", response.StatusCode)
	}
	var user map[string]any
	if err := json.NewDecoder(response.Body).Decode(&user); err != nil {
		t.Fatal(err)
	}
	updatedName := username + " updated"
	user["name"] = updatedName

	response = n.requestJSON(t, http.MethodPut, path, user, n.token)
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("update member status = %d", response.StatusCode)
	}

	response = n.requestJSON(t, http.MethodGet, path, nil, n.token)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("read updated member status = %d", response.StatusCode)
	}
	var updated struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	if updated.Name != updatedName {
		t.Fatalf("updated member name = %q, want %q", updated.Name, updatedName)
	}
}

func (n *realNavidrome) assignLibraries(t *testing.T, userID string, libraryIDs []int) {
	t.Helper()
	path := "/api/user/" + url.PathEscape(userID) + "/library/"
	response := n.requestJSON(t, http.MethodPut, path, map[string]any{"libraryIds": libraryIDs}, n.token)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("assign libraries status = %d", response.StatusCode)
	}
	_, _ = io.Copy(io.Discard, response.Body)
}

func (n *realNavidrome) verifyAssignedLibraries(t *testing.T, userID string, expected []int) {
	t.Helper()
	path := "/api/user/" + url.PathEscape(userID) + "/library/"
	response := n.requestJSON(t, http.MethodGet, path, nil, n.token)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("read assigned libraries status = %d", response.StatusCode)
	}
	var libraries []struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&libraries); err != nil {
		t.Fatal(err)
	}
	if len(libraries) != len(expected) {
		t.Fatalf("assigned libraries = %#v, want IDs %#v", libraries, expected)
	}
	for index := range expected {
		if libraries[index].ID != expected[index] {
			t.Fatalf("assigned library[%d] = %d, want %d", index, libraries[index].ID, expected[index])
		}
	}
}

func (n *realNavidrome) deleteNativeUser(t *testing.T, userID string) {
	t.Helper()
	if userID == "" {
		return
	}
	response := n.requestJSON(t, http.MethodDelete, "/api/user/"+url.PathEscape(userID)+"/", nil, n.token)
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNotFound {
		t.Errorf("cleanup user status = %d", response.StatusCode)
	}
}

func (n *realNavidrome) subsonicOK(t *testing.T, command string, values url.Values) map[string]any {
	t.Helper()
	if values == nil {
		values = make(url.Values)
	}
	values.Set("u", n.username)
	values.Set("jwt", n.token)
	values.Set("v", "1.16.1")
	values.Set("c", "flow-m0")
	values.Set("f", "json")
	response, err := n.http.Get(n.baseURL + "/rest/" + command + "?" + values.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Subsonic %s status = %d", command, response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	root, ok := payload["subsonic-response"].(map[string]any)
	if !ok || root["status"] != "ok" {
		t.Fatalf("Subsonic %s response = %#v", command, payload)
	}
	return root
}

func (n *realNavidrome) subsonicFailure(t *testing.T, command string, values url.Values) map[string]any {
	t.Helper()
	if values == nil {
		values = make(url.Values)
	}
	values.Set("u", n.username)
	values.Set("jwt", n.token)
	values.Set("v", "1.16.1")
	values.Set("c", "flow-m0")
	values.Set("f", "json")
	response, err := n.http.Get(n.baseURL + "/rest/" + command + "?" + values.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Subsonic %s HTTP status = %d", command, response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	root, ok := payload["subsonic-response"].(map[string]any)
	if !ok || root["status"] != "failed" {
		t.Fatalf("Subsonic %s response = %#v, want failed", command, payload)
	}
	return root
}

func (n *realNavidrome) subsonicHTTPStatus(t *testing.T, command string, values url.Values, expected int) {
	t.Helper()
	if values == nil {
		values = make(url.Values)
	}
	values.Set("u", n.username)
	values.Set("jwt", n.token)
	values.Set("v", "1.16.1")
	values.Set("c", "flow-m0")
	values.Set("f", "json")
	response, err := n.http.Get(n.baseURL + "/rest/" + command + "?" + values.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != expected {
		t.Fatalf("Subsonic %s HTTP status = %d, want %d", command, response.StatusCode, expected)
	}
}

func (n *realNavidrome) verifyMemberRoles(t *testing.T) {
	t.Helper()
	root := n.subsonicOK(t, "getUser", url.Values{"username": {n.username}})
	user, ok := root["user"].(map[string]any)
	if !ok {
		t.Fatalf("getUser response omitted user: %#v", root)
	}
	for _, field := range []string{"adminRole", "downloadRole", "shareRole", "coverArtRole"} {
		if value, ok := user[field].(bool); !ok || value {
			t.Fatalf("member %s = %#v, want false", field, user[field])
		}
	}
}

func (n *realNavidrome) verifyNativeUserAdminBoundary(t *testing.T) {
	t.Helper()
	response := n.requestJSON(t, http.MethodGet, "/api/user/", nil, n.token)
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("member native user-list status = %d, want 403", response.StatusCode)
	}
}

func (n *realNavidrome) waitForScan(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		root := n.subsonicOK(t, "getScanStatus", nil)
		status, _ := root["scanStatus"].(map[string]any)
		if scanning, _ := status["scanning"].(bool); !scanning {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("Navidrome scan did not finish")
}

func (n *realNavidrome) firstSong(t *testing.T) (string, string) {
	t.Helper()
	root := n.subsonicOK(t, "getRandomSongs", url.Values{"size": {"1"}})
	randomSongs, _ := root["randomSongs"].(map[string]any)
	songs, _ := randomSongs["song"].([]any)
	if len(songs) == 0 {
		return "", ""
	}
	song, _ := songs[0].(map[string]any)
	id, _ := song["id"].(string)
	coverArt, _ := song["coverArt"].(string)
	return id, coverArt
}

func (n *realNavidrome) verifyDirectPlaylistWriteSurface(t *testing.T) {
	t.Helper()
	root := n.subsonicOK(t, "createPlaylist", url.Values{"name": {"flow-m0-contract"}})
	playlist, _ := root["playlist"].(map[string]any)
	playlistID, _ := playlist["id"].(string)
	if playlistID == "" {
		t.Fatal("member createPlaylist succeeded without a playlist id")
	}
	// Navidrome permits the member-scoped write. Flow must therefore enforce the
	// product's admin-only playlist mutation rule at its own boundary.
	n.subsonicOK(t, "deletePlaylist", url.Values{"id": {playlistID}})
}

func (n *realNavidrome) verifyOriginalRange(t *testing.T, mediaID string) {
	t.Helper()
	endpoint := n.streamURL(mediaID, url.Values{"format": {"raw"}})
	request, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	request.Header.Set("Range", "bytes=0-31")
	response, err := n.http.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("original Range status = %d, want 206", response.StatusCode)
	}
	if response.Header.Get("Content-Range") == "" || !strings.EqualFold(response.Header.Get("Accept-Ranges"), "bytes") {
		t.Fatalf("original Range headers: Content-Range=%q Accept-Ranges=%q", response.Header.Get("Content-Range"), response.Header.Get("Accept-Ranges"))
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "audio/") {
		t.Fatalf("original Content-Type = %q, want audio/*", contentType)
	}
}

func (n *realNavidrome) verifyTranscode(t *testing.T, mediaID, format string) {
	t.Helper()
	endpoint := n.streamURL(mediaID, url.Values{"format": {format}, "maxBitRate": {"64"}, "timeOffset": {"1"}})
	request, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	request.Header.Set("Range", "bytes=0-31")
	response, err := n.http.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 64<<10)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s transcode status = %d, want 200", format, response.StatusCode)
	}
	if got := response.Header.Get("Accept-Ranges"); got != "none" {
		t.Fatalf("%s transcode Accept-Ranges = %q, want none", format, got)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "audio/") {
		t.Fatalf("%s transcode Content-Type = %q, want audio/*", format, contentType)
	}
}

func (n *realNavidrome) streamURL(mediaID string, extra url.Values) string {
	values := make(url.Values)
	for key, items := range extra {
		for _, item := range items {
			values.Add(key, item)
		}
	}
	values.Set("id", mediaID)
	values.Set("u", n.username)
	values.Set("jwt", n.token)
	values.Set("v", "1.16.1")
	values.Set("c", "flow-m0")
	return n.baseURL + "/rest/stream?" + values.Encode()
}

func (n *realNavidrome) requestJSON(t *testing.T, method, path string, body any, token string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, n.baseURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set(ndAuthorizationHeader, "Bearer "+token)
	}
	response, err := n.http.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if refreshed := response.Header.Get(ndAuthorizationHeader); strings.HasPrefix(refreshed, "Bearer ") {
		n.token = strings.TrimPrefix(refreshed, "Bearer ")
	}
	return response
}

func TestPinnedVersionDeclaration(t *testing.T) {
	if got := os.Getenv("FLOW_REAL_NAVIDROME_VERSION"); got != "" && got != "0.64.2" {
		t.Fatalf("contract suite requires Navidrome 0.64.2, got %s", got)
	}
	t.Log(fmt.Sprintf("Navidrome contract target: %s", "0.64.2"))
}
