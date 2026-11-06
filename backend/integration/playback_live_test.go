//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/database"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/playback"
)

// This gate runs the real flowd executable, real Flow cookie authentication,
// PostgreSQL, member provisioning, pinned Navidrome and the Flow audio gateway.
// Browser assertions are enabled by FLOW_BROWSER_EXECUTABLE and use the built
// web bundle; no mocked media or database can count as a pass here.
func TestM3LivePlayback(t *testing.T) {
	dsn := os.Getenv("FLOW_TEST_DATABASE_URL")
	ndURL, ndUsername, ndPassword := os.Getenv("FLOW_REAL_NAVIDROME_URL"), os.Getenv("FLOW_REAL_NAVIDROME_ADMIN_USERNAME"), os.Getenv("FLOW_REAL_NAVIDROME_ADMIN_PASSWORD")
	if dsn == "" || ndURL == "" || ndUsername == "" || ndPassword == "" || os.Getenv("FLOW_REAL_NAVIDROME_ALLOW_MUTATION") != "true" {
		t.Skip("M3 live gate requires disposable PostgreSQL and explicitly disposable pinned Navidrome")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	nd := &realNavidrome{baseURL: strings.TrimRight(ndURL, "/"), username: ndUsername, password: ndPassword, http: &http.Client{Timeout: 20 * time.Second}}
	if os.Getenv("FLOW_REAL_NAVIDROME_CREATE_ADMIN") == "true" {
		status := nd.createAdmin(t)
		if status != 200 && status != 403 {
			t.Fatalf("disposable Navidrome bootstrap returned %d", status)
		}
	}
	nd.login(t)
	nd.verifyServerVersion(t, "0.64.2")
	nd.subsonicOK(t, "startScan", nil)
	nd.waitForScan(t)

	base, err := database.Open(ctx, dsn, 2)
	if err != nil {
		t.Fatal(err)
	}
	schema := "flow_m3_live_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := base.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		base.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = base.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE")
		base.Close()
	})
	schemaURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := schemaURL.Query()
	query.Set("search_path", schema)
	schemaURL.RawQuery = query.Encode()
	backendRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Dir(backendRoot)
	temporary := t.TempDir()
	binary := filepath.Join(temporary, "flowd")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/flowd")
	build.Dir = backendRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build live Flow executable: %v\n%s", err, output)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	keyring := filepath.Join(temporary, "keyring.json")
	keyJSON, _ := json.Marshal(map[string]any{"activeVersion": 1, "keys": map[string]string{"1": base64.StdEncoding.EncodeToString(key)}})
	if err := os.WriteFile(keyring, keyJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	// The test-only static server provides the built SPA and same-origin proxy.
	// All production APIs and audio bytes are served by the actual flowd child.
	var proxy atomic.Pointer[httputil.ReverseProxy]
	static := http.FileServer(http.Dir(filepath.Join(repoRoot, "web", "dist")))
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/audio/") {
			if target := proxy.Load(); target != nil {
				target.ServeHTTP(w, r)
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		static.ServeHTTP(w, r)
	}))
	defer front.Close()
	flowPassword := "flow-m3-admin-test-password"
	flowEnv := map[string]string{
		"FLOW_HTTP_ADDR": "127.0.0.1:0", "FLOW_DATABASE_URL": schemaURL.String(),
		"FLOW_NAVIDROME_URL": ndURL, "FLOW_NAVIDROME_ADMIN_USERNAME": ndUsername, "FLOW_NAVIDROME_ADMIN_PASSWORD": ndPassword,
		"FLOW_ADMIN_USERNAME": "flow-m3-admin", "FLOW_ADMIN_PASSWORD": flowPassword,
		"FLOW_CREDENTIAL_KEYRING_FILE": keyring, "FLOW_PUBLIC_ORIGIN": front.URL,
		"FLOW_SESSION_TTL": "8h", "FLOW_PROVISION_TIMEOUT": "30s", "FLOW_LOG_LEVEL": "info",
		"FLOW_HTTP_READ_HEADER_TIMEOUT": "5s", "FLOW_HTTP_IDLE_TIMEOUT": "60s", "FLOW_SHUTDOWN_TIMEOUT": "10s",
		"FLOW_NAVIDROME_TIMEOUT": "5s", "FLOW_NAVIDROME_STREAM_HEADER_TIMEOUT": "10s",
		"FLOW_MIGRATION_TIMEOUT": "30s", "FLOW_DATABASE_MAX_CONNS": "10",
	}
	current := startLiveFlow(t, ctx, binary, flowEnv)
	defer func() { current.stop(t) }()
	setProxy := func() {
		target, _ := url.Parse(current.url)
		proxy.Store(httputil.NewSingleHostReverseProxy(target))
	}
	setProxy()
	admin := liveClient(t)
	liveJSON(t, admin, front.URL, "POST", "/api/auth/login", map[string]any{"username": "flow-m3-admin", "password": flowPassword}, 200)
	var users []string
	var userIDs []int64
	memberPassword := "flow-m3-member-test-password"
	for index := range 2 {
		name := "m3_" + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.Itoa(index)
		var created struct {
			User struct {
				ID int64 `json:"id"`
			} `json:"user"`
		}
		data := liveJSON(t, admin, front.URL, "POST", "/api/admin/users", map[string]any{"username": name, "displayName": name, "temporaryPassword": "flow-m3-temporary-password"}, 201)
		if json.Unmarshal(data, &created) != nil || created.User.ID < 1 {
			t.Fatal("real member creation did not return its Flow account ID")
		}
		userIDs = append(userIDs, created.User.ID)
		users = append(users, name)
		member := liveClient(t)
		liveJSON(t, member, front.URL, "POST", "/api/auth/login", map[string]any{"username": name, "password": "flow-m3-temporary-password"}, 200)
		liveJSON(t, member, front.URL, "GET", "/api/playback/session", nil, 403)
		liveJSON(t, member, front.URL, "POST", "/api/auth/change-password", map[string]any{"currentPassword": "flow-m3-temporary-password", "newPassword": memberPassword}, 204)
	}
	// Disposable members are deleted through Flow while its process is still up.
	defer func() {
		for _, id := range userIDs {
			r, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/api/admin/users/%d?hard=true", front.URL, id), nil)
			r.Header.Set("Origin", front.URL)
			response, err := admin.Do(r)
			if err != nil {
				t.Errorf("disposable member cleanup failed: %v", err)
				continue
			}
			_ = response.Body.Close()
			if response.StatusCode != 204 {
				t.Errorf("disposable member cleanup returned %d", response.StatusCode)
			}
		}
	}()
	member := liveClient(t)
	liveJSON(t, member, front.URL, "POST", "/api/auth/login", map[string]any{"username": users[0], "password": memberPassword}, 200)
	var songs library.SongPage
	if err := json.Unmarshal(liveJSON(t, member, front.URL, "GET", "/api/library/songs", nil, 200), &songs); err != nil || len(songs.Items) == 0 {
		t.Fatal("authenticated member could not browse the real library")
	}
	mediaID := songs.Items[0].ID
	get := func(client *http.Client) playback.Session {
		var state playback.Session
		if err := json.Unmarshal(liveJSON(t, client, front.URL, "GET", "/api/playback/session", nil, 200), &state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	mutate := func(body map[string]any) playback.Session {
		body["ifRevision"] = get(admin).Revision
		var result playback.Session
		if err := json.Unmarshal(liveJSON(t, admin, front.URL, "PUT", "/api/playback/session", body, 200), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	state := mutate(map[string]any{"queue": []map[string]string{{"mediaId": mediaID}, {"mediaId": mediaID}}, "queueIndex": 0, "status": "playing"})
	state = mutate(map[string]any{"command": "checkpoint", "currentMediaId": mediaID, "queueIndex": 0, "positionMs": 1000})
	state = mutate(map[string]any{"status": "paused", "positionMs": 2000})
	state = mutate(map[string]any{"queueEdit": map[string]any{"move": map[string]int{"from": 0, "to": 1}}})
	if state.QueueIndex != 1 || state.PositionMS != 2000 {
		t.Fatal("real queue reorder lost selected occurrence or position")
	}
	state = mutate(map[string]any{"command": "previous", "positionMs": 0})
	if state.QueueIndex != 0 || state.PositionMS != 0 {
		t.Fatal("real previous command did not select the prior occurrence")
	}
	state = mutate(map[string]any{"command": "next"})
	if state.QueueIndex != 1 {
		t.Fatal("real next command did not advance")
	}
	state = mutate(map[string]any{"volume": .6, "shuffle": true})
	if other := get(member); len(other.Queue) != 0 || other.Revision != 0 || other.Volume != 1 {
		t.Fatal("administrator controls changed a member session")
	}
	liveJSON(t, member, front.URL, "GET", "/api/playback/session?userId=1", nil, 400)
	liveJSON(t, member, front.URL, "PUT", "/api/playback/session", map[string]any{"ifRevision": 0, "userId": 1, "volume": .1}, 400)
	liveJSON(t, liveClient(t), front.URL, "GET", "/api/playback/session", nil, 401)
	// Both independently authenticated clients submit the same revision.
	secondAdmin := liveClient(t)
	liveJSON(t, secondAdmin, front.URL, "POST", "/api/auth/login", map[string]any{"username": "flow-m3-admin", "password": flowPassword}, 200)
	results := make(chan int, 2)
	ready := make(chan struct{})
	var wait sync.WaitGroup
	for index, client := range []*http.Client{admin, secondAdmin} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-ready
			body, _ := json.Marshal(map[string]any{"ifRevision": state.Revision, "volume": float64(index+1) / 10})
			r, _ := http.NewRequestWithContext(ctx, "PUT", front.URL+"/api/playback/session", bytes.NewReader(body))
			r.Header.Set("Origin", front.URL)
			r.Header.Set("Content-Type", "application/json")
			response, err := client.Do(r)
			if err != nil {
				results <- 0
				return
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			results <- response.StatusCode
		}()
	}
	close(ready)
	wait.Wait()
	first, second := <-results, <-results
	if !(first == 200 && second == 409 || first == 409 && second == 200) {
		t.Fatalf("real concurrent HTTP writes returned %d and %d", first, second)
	}
	state = get(admin)
	streamRequest, _ := http.NewRequestWithContext(ctx, "GET", front.URL+"/audio/"+mediaID+"?format=original", nil)
	streamRequest.Header.Set("Range", "bytes=0-31")
	stream, err := member.Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	audioBytes, err := io.ReadAll(io.LimitReader(stream.Body, 64))
	_ = stream.Body.Close()
	if err != nil || stream.StatusCode != 206 || len(audioBytes) != 32 || stream.Header.Get("Content-Range") == "" {
		t.Fatal("authenticated real Flow gateway did not return the original range")
	}

	var browserState []byte
	if executable := os.Getenv("FLOW_BROWSER_EXECUTABLE"); executable != "" {
		if _, err := os.Stat(filepath.Join(repoRoot, "web", "dist", "index.html")); err != nil {
			t.Fatal("build the web bundle before running the live browser gate")
		}
		browserState = runLiveBrowser(t, ctx, repoRoot, front.URL, users, memberPassword, nil)
	} else {
		t.Run("browser gate", func(t *testing.T) { t.Skip("FLOW_BROWSER_EXECUTABLE is unset; real browser playback is unverified") })
	}
	proxy.Store(nil)
	current.stop(t)
	current = startLiveFlow(t, ctx, binary, flowEnv)
	setProxy()
	if restored := get(admin); !reflect.DeepEqual(state, restored) {
		t.Fatal("playback state did not survive an actual flowd process restart")
	}
	if len(browserState) != 0 {
		runLiveBrowser(t, ctx, repoRoot, front.URL, users, memberPassword, browserState)
	}
	liveJSON(t, admin, front.URL, "POST", fmt.Sprintf("/api/admin/users/%d/suspend", userIDs[0]), map[string]any{}, 204)
	liveJSON(t, member, front.URL, "GET", "/api/playback/session", nil, 401)
	liveJSON(t, member, front.URL, "GET", "/audio/"+mediaID, nil, 401)
	liveJSON(t, member, front.URL, "POST", "/api/auth/login", map[string]any{"username": users[0], "password": memberPassword}, 403)
	t.Log("real Flow authentication, PostgreSQL persistence, member-scoped Navidrome library/audio, HTTP CAS, isolation and actual Flow process restart passed")
}

type liveFlowProcess struct {
	command *exec.Cmd
	done    chan error
	url     string
	stopped bool
}

func startLiveFlow(t *testing.T, ctx context.Context, binary string, values map[string]string) *liveFlowProcess {
	t.Helper()
	command := exec.CommandContext(ctx, binary)
	command.Env = liveEnvironment(values)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &liveFlowProcess{command: command, done: make(chan error, 1)}
	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			var log struct {
				Message string `json:"msg"`
				Address string `json:"address"`
			}
			if json.Unmarshal(scanner.Bytes(), &log) == nil && log.Message == "flow backend listening" {
				address <- log.Address
			}
		}
	}()
	go func() { process.done <- command.Wait() }()
	select {
	case addr := <-address:
		process.url = "http://" + addr
	case <-process.done:
		process.stopped = true
		t.Fatal("live flowd exited during bootstrap; verify disposable dependency configuration")
	case <-time.After(30 * time.Second):
		process.stop(t)
		t.Fatal("live flowd did not finish bootstrap")
	}
	return process
}

func (p *liveFlowProcess) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	_ = p.command.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-p.done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("live flowd stopped with an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = p.command.Process.Kill()
		<-p.done
		t.Error("live flowd shutdown timed out")
	}
}

func liveEnvironment(values map[string]string) []string {
	result := make([]string, 0, len(os.Environ())+len(values))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if _, replaced := values[name]; !replaced {
			result = append(result, value)
		}
	}
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}

func liveClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 30 * time.Second}
}

func liveJSON(t *testing.T, client *http.Client, origin, method, path string, body any, status int) []byte {
	t.Helper()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	r, err := http.NewRequest(method, origin+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/json")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != status {
		t.Fatalf("live %s %s status=%d (expected %d), read error=%v", method, path, response.StatusCode, status, err)
	}
	return data
}

func runLiveBrowser(t *testing.T, ctx context.Context, root, origin string, users []string, password string, expected []byte) []byte {
	t.Helper()
	command := exec.CommandContext(ctx, "node", "tests/browser.mjs")
	command.Dir = filepath.Join(root, "web")
	command.Env = liveEnvironment(map[string]string{
		"FLOW_BROWSER_TEST_URL": origin, "FLOW_BROWSER_TEST_USERNAME": users[0], "FLOW_BROWSER_TEST_PASSWORD": password,
		"FLOW_BROWSER_OTHER_USERNAME": users[1], "FLOW_BROWSER_OTHER_PASSWORD": password,
		"FLOW_M3_EXPECTED_SESSION": string(expected),
	})
	// The test client receives Flow login inputs, never server-side Navidrome
	// credentials, keyring locations or database connection details.
	filtered := command.Env[:0]
	for _, value := range command.Env {
		name, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(name, "FLOW_") || strings.HasPrefix(name, "FLOW_BROWSER_") || name == "FLOW_M3_EXPECTED_SESSION" {
			filtered = append(filtered, value)
		}
	}
	command.Env = filtered
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real M3 browser assertions failed: %v\n%s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "M3_SAVED_SESSION=") {
			return []byte(strings.TrimPrefix(line, "M3_SAVED_SESSION="))
		}
		if strings.HasPrefix(line, "PASS ") {
			t.Log(line)
		}
	}
	if len(expected) == 0 {
		t.Fatal("browser gate did not return its last durable session")
	}
	return expected
}
