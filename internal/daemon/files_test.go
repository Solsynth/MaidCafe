package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// fileDaemon starts a daemon with the file API enabled on [root].
func fileDaemon(t *testing.T, root string, mutate func(*config.FilesConfig)) (*App, string) {
	t.Helper()
	files := config.FilesConfig{
		Enabled:    true,
		Secret:     "files-secret",
		Roots:      []string{root},
		AllowWrite: true,
	}
	if mutate != nil {
		mutate(&files)
	}
	cfg := config.DaemonConfig{
		ID:                "host-files",
		Transport:         "http",
		Listen:            "127.0.0.1:0",
		MetricsSecret:     "metrics-secret",
		MetricsInterval:   time.Hour,
		StreamInterval:    time.Second,
		Runtimes:          []string{"java"},
		ProcessesLimit:    50,
		RequestTimeout:    5 * time.Second,
		ScriptTimeout:     time.Second,
		MaxBodyBytes:      1024,
		MaxConcurrentRuns: 1,
		AuditPath:         filepath.Join(t.TempDir(), "audit.jsonl"),
		Files:             files,
	}
	app, err := NewApp(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = app.Shutdown(context.Background())
	})
	return app, "http://" + app.ListenAddr()
}

// fileJSON performs one authenticated JSON call against the file API and
// decodes the response body.
func fileJSON(t *testing.T, method, url string, body []byte, mutate func(*http.Request)) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer files-secret")
	if len(body) > 0 {
		request.Header.Set("X-MaidCafe-Signature", signedHeader("metrics-secret", body))
	}
	if mutate != nil {
		mutate(request)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode %s %s: %v (%s)", method, url, err, raw)
		}
	}
	return response.StatusCode, decoded
}

func TestFileAPIReadWriteRoundtripAndAudit(t *testing.T) {
	root := t.TempDir()
	app, base := fileDaemon(t, root, nil)
	payload := []byte("hello file api\n")

	writeBody, err := json.Marshal(map[string]any{
		"path":    filepath.Join(root, "notes.txt"),
		"content": base64.StdEncoding.EncodeToString(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	status, response := fileJSON(t, http.MethodPost, base+"/api/v1/files/write", writeBody, nil)
	if status != http.StatusOK {
		t.Fatalf("write status = %d (%v)", status, response)
	}
	if response["size"].(float64) != float64(len(payload)) {
		t.Fatalf("write response = %v", response)
	}

	status, response = fileJSON(t, http.MethodPost, base+"/api/v1/files/read", []byte(`{"path":`+
		strconv.Quote(filepath.Join(root, "notes.txt"))+`}`), nil)
	if status != http.StatusOK {
		t.Fatalf("read status = %d (%v)", status, response)
	}
	content, err := base64.StdEncoding.DecodeString(response["content"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, payload) {
		t.Fatalf("read content = %q", content)
	}

	// The audit trail names the action, the transport and the path.
	entries := app.audit.Recent(10)
	if len(entries) < 2 {
		t.Fatalf("audit entries = %d", len(entries))
	}
	latest := entries[0]
	if latest.Name != fileActionRead || latest.Source != fileSourceHTTP || !latest.OK {
		t.Fatalf("audit entry = %#v", latest)
	}
	if latest.Target != filepath.Join(root, "notes.txt") {
		t.Fatalf("audit target = %q", latest.Target)
	}
}

func TestFileAPIContentRouteStreamsWindow(t *testing.T) {
	root := t.TempDir()
	content := []byte("0123456789")
	if err := os.WriteFile(filepath.Join(root, "digits.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	_, base := fileDaemon(t, root, nil)

	// The raw route takes the credential from the query string, which is what
	// a browser download or an image element can actually carry.
	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/files/content?token=files-secret&path="+
		urlQuery(filepath.Join(root, "digits.txt"))+"&offset=2&limit=3", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", response.StatusCode, body)
	}
	if string(body) != "234" {
		t.Fatalf("window = %q", body)
	}
	if response.Header.Get("X-MaidCafe-File-Size") != "10" {
		t.Fatalf("size header = %q", response.Header.Get("X-MaidCafe-File-Size"))
	}

	// A wrong token is refused, and the refusal does not depend on the path.
	request, err = http.NewRequest(http.MethodGet, base+"/api/v1/files/content?token=nope&path="+
		urlQuery(filepath.Join(root, "digits.txt")), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status = %d", response.StatusCode)
	}
}

func urlQuery(value string) string {
	return strings.NewReplacer("/", "%2F", " ", "%20").Replace(value)
}

func TestFileAPIRefusesPathsOutsideRoots(t *testing.T) {
	root := t.TempDir()
	_, base := fileDaemon(t, root, nil)

	outside := filepath.Join(filepath.Dir(root), "elsewhere")
	for _, path := range []string{"/etc/hosts", outside, root + "/../" + filepath.Base(outside)} {
		status, response := fileJSON(t, http.MethodGet, base+"/api/v1/files/list?path="+urlQuery(path), nil, nil)
		if status != http.StatusForbidden {
			t.Fatalf("list %q status = %d (%v)", path, status, response)
		}
	}
	status, _ := fileJSON(t, http.MethodGet, base+"/api/v1/files/list?path=relative", nil, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("relative path status = %d", status)
	}
}

func TestFileAPIRefusesSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	_, base := fileDaemon(t, root, nil)

	// The requested path is lexically inside the root, so only os.Root stands
	// between the caller and the file outside it.
	status, response := fileJSON(t, http.MethodGet,
		base+"/api/v1/files/read?path="+urlQuery(filepath.Join(root, "escape", "secret.txt")), nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("symlink escape status = %d (%v)", status, response)
	}
	// The link itself is still visible, with its destination named and no
	// followable target type.
	status, response = fileJSON(t, http.MethodGet,
		base+"/api/v1/files/stat?path="+urlQuery(filepath.Join(root, "escape"))+"&follow=false", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("stat link status = %d (%v)", status, response)
	}
	if response["type"] != "symlink" || response["link_target"] != outside {
		t.Fatalf("stat link = %v", response)
	}
	if _, ok := response["target_type"]; ok {
		t.Fatalf("escape target should not resolve: %v", response)
	}
	// Asking to follow it is an explicit refusal rather than a silent answer.
	status, _ = fileJSON(t, http.MethodGet,
		base+"/api/v1/files/stat?path="+urlQuery(filepath.Join(root, "escape")), nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("follow escape status = %d", status)
	}
}

func TestFileAPIReadWindowAndCaps(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.bin"), bytes.Repeat([]byte("a"), 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	_, base := fileDaemon(t, root, func(f *config.FilesConfig) {
		f.MaxReadBytes = 1024
		f.MaxWriteBytes = 1024
	})

	// A whole-file read of a file past the cap is refused, never truncated.
	status, response := fileJSON(t, http.MethodGet, base+"/api/v1/files/read?path="+urlQuery(filepath.Join(root, "big.bin")), nil, nil)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized read status = %d (%v)", status, response)
	}
	// An explicit window works and reports the file's real size, so a client
	// can page the rest.
	status, response = fileJSON(t, http.MethodGet, base+"/api/v1/files/read?path="+urlQuery(filepath.Join(root, "big.bin"))+"&offset=1000&limit=24", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("window read status = %d (%v)", status, response)
	}
	content, err := base64.StdEncoding.DecodeString(response["content"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != 24 || response["size"].(float64) != 4096 || response["offset"].(float64) != 1000 {
		t.Fatalf("window read = %d bytes, size %v, offset %v", len(content), response["size"], response["offset"])
	}
	// An offset past the end is a definite rejection.
	status, _ = fileJSON(t, http.MethodGet, base+"/api/v1/files/read?path="+urlQuery(filepath.Join(root, "big.bin"))+"&offset=99999", nil, nil)
	if status != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("offset past end status = %d", status)
	}
	// Writing more than the write cap is refused by the raw route.
	request, err := http.NewRequest(http.MethodPut, base+"/api/v1/files/content?path="+
		urlQuery(filepath.Join(root, "upload.bin")), bytes.NewReader(bytes.Repeat([]byte("b"), 2048)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer files-secret")
	response2, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response2.Body.Close()
	if response2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized write status = %d", response2.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "upload.bin")); !os.IsNotExist(err) {
		t.Fatal("an oversized write must not leave a file behind")
	}
}

func TestFileAPIWriteRequiresSignatureAndWriteOptIn(t *testing.T) {
	root := t.TempDir()
	_, base := fileDaemon(t, root, nil)

	body := []byte(`{"path":` + strconv.Quote(filepath.Join(root, "unsigned.txt")) + `,"content":"aGk="}`)
	// The bearer credential alone is not enough for a mutation: the body must
	// be bound by the signature too.
	status, _ := fileJSON(t, http.MethodPost, base+"/api/v1/files/write", body, func(r *http.Request) {
		r.Header.Del("X-MaidCafe-Signature")
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("unsigned write status = %d", status)
	}
	// A signature over a different body does not transfer.
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/write", body, func(r *http.Request) {
		r.Header.Set("X-MaidCafe-Signature", signedHeader("metrics-secret", []byte("other")))
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("mismatched signature status = %d", status)
	}

	// A read-only policy serves reads and refuses every mutation.
	readOnlyRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(readOnlyRoot, "visible.txt"), []byte("shown"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, readOnlyBase := fileDaemon(t, readOnlyRoot, func(f *config.FilesConfig) { f.AllowWrite = false })
	status, _ = fileJSON(t, http.MethodGet, readOnlyBase+"/api/v1/files/list?path="+urlQuery(readOnlyRoot), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read-only list status = %d", status)
	}
	writeBody := []byte(`{"path":` + strconv.Quote(filepath.Join(readOnlyRoot, "new.txt")) + `,"content":"aGk="}`)
	status, response := fileJSON(t, http.MethodPost, readOnlyBase+"/api/v1/files/write", writeBody, nil)
	if status != http.StatusForbidden {
		t.Fatalf("read-only write status = %d (%v)", status, response)
	}
	status, _ = fileJSON(t, http.MethodPost, readOnlyBase+"/api/v1/files/delete", []byte(`{"path":`+
		strconv.Quote(filepath.Join(readOnlyRoot, "visible.txt"))+`}`), nil)
	if status != http.StatusForbidden {
		t.Fatalf("read-only delete status = %d", status)
	}
}

func TestFileAPIListStatMoveCopyDelete(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "file.txt"), []byte("body"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file.txt", filepath.Join(root, "dir", "link")); err != nil {
		t.Fatal(err)
	}
	_, base := fileDaemon(t, root, nil)

	status, response := fileJSON(t, http.MethodGet, base+"/api/v1/files/list?path="+urlQuery(filepath.Join(root, "dir")), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d (%v)", status, response)
	}
	entries := response["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("entries = %v", entries)
	}
	byName := map[string]map[string]any{}
	for _, raw := range entries {
		entry := raw.(map[string]any)
		byName[entry["name"].(string)] = entry
	}
	if byName["nested"]["type"] != "directory" {
		t.Fatalf("nested entry = %v", byName["nested"])
	}
	fileEntry := byName["file.txt"]
	if fileEntry["type"] != "file" || fileEntry["size"].(float64) != 4 || fileEntry["mode"].(float64) != 0o640 {
		t.Fatalf("file entry = %v", fileEntry)
	}
	linkEntry := byName["link"]
	if linkEntry["type"] != "symlink" || linkEntry["link_target"] != "file.txt" || linkEntry["target_type"] != "file" {
		t.Fatalf("link entry = %v", linkEntry)
	}

	// move renames a file inside one root.
	moveBody := []byte(`{"from":` + strconv.Quote(filepath.Join(root, "dir", "file.txt")) + `,"to":` +
		strconv.Quote(filepath.Join(root, "dir", "renamed.txt")) + `}`)
	status, response = fileJSON(t, http.MethodPost, base+"/api/v1/files/move", moveBody, nil)
	if status != http.StatusOK {
		t.Fatalf("move status = %d (%v)", status, response)
	}
	if _, err := os.Stat(filepath.Join(root, "dir", "renamed.txt")); err != nil {
		t.Fatal(err)
	}

	// copy duplicates a whole tree, links included.
	copyBody := []byte(`{"from":` + strconv.Quote(filepath.Join(root, "dir")) + `,"to":` +
		strconv.Quote(filepath.Join(root, "copy")) + `}`)
	status, response = fileJSON(t, http.MethodPost, base+"/api/v1/files/copy", copyBody, nil)
	if status != http.StatusOK {
		t.Fatalf("copy status = %d (%v)", status, response)
	}
	copied, err := os.ReadFile(filepath.Join(root, "copy", "renamed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != "body" {
		t.Fatalf("copied content = %q", copied)
	}
	target, err := os.Readlink(filepath.Join(root, "copy", "link"))
	if err != nil || target != "file.txt" {
		t.Fatalf("copied link = %q, %v", target, err)
	}
	// A second copy onto the existing destination needs overwrite.
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/copy", copyBody, nil)
	if status != http.StatusConflict {
		t.Fatalf("repeat copy status = %d", status)
	}
	overwriteBody := []byte(`{"from":` + strconv.Quote(filepath.Join(root, "dir")) + `,"to":` +
		strconv.Quote(filepath.Join(root, "copy")) + `,"overwrite":true}`)
	status, response = fileJSON(t, http.MethodPost, base+"/api/v1/files/copy", overwriteBody, nil)
	if status != http.StatusOK {
		t.Fatalf("overwrite copy status = %d (%v)", status, response)
	}
	// Copying a directory into itself would recurse forever.
	intoSelf := []byte(`{"from":` + strconv.Quote(filepath.Join(root, "dir")) + `,"to":` +
		strconv.Quote(filepath.Join(root, "dir", "inner")) + `}`)
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/copy", intoSelf, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("copy into source status = %d", status)
	}

	// A configured root is never deletable, and a non-empty directory needs
	// the recursive flag.
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/delete", []byte(`{"path":`+strconv.Quote(root)+`}`), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("delete root status = %d", status)
	}
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/delete", []byte(`{"path":`+
		strconv.Quote(filepath.Join(root, "copy"))+`}`), nil)
	if status != http.StatusConflict {
		t.Fatalf("delete non-empty status = %d", status)
	}
	status, response = fileJSON(t, http.MethodPost, base+"/api/v1/files/delete", []byte(`{"path":`+
		strconv.Quote(filepath.Join(root, "copy"))+`,"recursive":true}`), nil)
	if status != http.StatusOK {
		t.Fatalf("recursive delete status = %d (%v)", status, response)
	}
	if _, err := os.Stat(filepath.Join(root, "copy")); !os.IsNotExist(err) {
		t.Fatal("recursive delete left the directory behind")
	}
}

func TestFileAPIRootsAndDisabledRefusal(t *testing.T) {
	root := t.TempDir()
	_, base := fileDaemon(t, root, nil)
	status, response := fileJSON(t, http.MethodGet, base+"/api/v1/files/roots", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("roots status = %d", status)
	}
	roots := response["roots"].([]any)
	if len(roots) != 1 || roots[0].(map[string]any)["path"] != root || response["writable"] != true {
		t.Fatalf("roots = %v", response)
	}

	// A daemon with the API off refuses everything, including roots.
	disabledRoot := t.TempDir()
	disabled, disabledBase := fileDaemon(t, disabledRoot, func(f *config.FilesConfig) { *f = config.FilesConfig{} })
	status, _ = fileJSON(t, http.MethodGet, disabledBase+"/api/v1/files/roots", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("disabled roots status = %d", status)
	}
	if entries := disabled.audit.Recent(5); len(entries) != 0 {
		t.Fatalf("disabled API wrote audit entries: %#v", entries)
	}
}

func TestFileAPIListTruncatesAtCap(t *testing.T) {
	root := t.TempDir()
	for i := range 5 {
		name := filepath.Join(root, "file-"+strconv.Itoa(i)+".txt")
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, base := fileDaemon(t, root, func(f *config.FilesConfig) { f.MaxListEntries = 3 })
	status, response := fileJSON(t, http.MethodGet, base+"/api/v1/files/list?path="+urlQuery(root), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	if entries := response["entries"].([]any); len(entries) != 3 {
		t.Fatalf("entries = %d", len(entries))
	}
	if response["truncated"] != true {
		t.Fatalf("truncated = %v", response["truncated"])
	}
}

func TestFileAPISubprotocolCredential(t *testing.T) {
	root := t.TempDir()
	_, base := fileDaemon(t, root, nil)
	token := fileTokenSubprotocolPrefix + base64.RawURLEncoding.EncodeToString([]byte("files-secret"))
	status, response := fileJSON(t, http.MethodGet, base+"/api/v1/files/roots", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		r.Header.Set("Sec-WebSocket-Protocol", token)
	})
	if status != http.StatusOK {
		t.Fatalf("subprotocol status = %d (%v)", status, response)
	}
	status, _ = fileJSON(t, http.MethodGet, base+"/api/v1/files/roots", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		r.Header.Set("Sec-WebSocket-Protocol", fileTokenSubprotocolPrefix+
			base64.RawURLEncoding.EncodeToString([]byte("wrong")))
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong subprotocol status = %d", status)
	}
}

func TestFileAPIQueryTokenReadsOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, base := fileDaemon(t, root, nil)

	// A raw read may carry the credential in the URL: a browser cannot put a
	// header on a download or an image load.
	response, err := http.Get(base + "/api/v1/files/content?token=files-secret&path=" + urlQuery(filepath.Join(root, "file.txt")))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("content with query token status = %d", response.StatusCode)
	}

	// Every other route refuses the query token: a leaked URL must not be
	// replayable into a write or a delete.
	writeBody := []byte(`{"path":` + strconv.Quote(filepath.Join(root, "file.txt")) + `,"content":"aGk="}`)
	status, _ := fileJSON(t, http.MethodPost, base+"/api/v1/files/write?token=files-secret", writeBody, func(r *http.Request) {
		r.Header.Del("Authorization")
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("write with query token status = %d", status)
	}
	putRequest, err := http.NewRequest(http.MethodPut,
		base+"/api/v1/files/content?token=files-secret&path="+urlQuery(filepath.Join(root, "file.txt")),
		bytes.NewReader([]byte("clobbered")))
	if err != nil {
		t.Fatal(err)
	}
	putResponse, err := http.DefaultClient.Do(putRequest)
	if err != nil {
		t.Fatal(err)
	}
	putResponse.Body.Close()
	if putResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("raw write with query token status = %d", putResponse.StatusCode)
	}
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/delete?token=files-secret&path="+
		urlQuery(filepath.Join(root, "file.txt")), nil, func(r *http.Request) {
		r.Header.Del("Authorization")
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("delete with query token status = %d", status)
	}
	content, err := os.ReadFile(filepath.Join(root, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "body" {
		t.Fatalf("file changed through a query credential: %q", content)
	}
}

func TestFileAPIPolicyHotReload(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootA, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "b.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	writeConfig := func(files string) {
		t.Helper()
		text := `[daemon]
id = "host-reload"
transport = "http"
listen = "127.0.0.1:0"
metricsSecret = "metrics-secret"
metricsHistoryPath = ""
metricsInterval = "1m"
streamInterval = "1s"
runtimes = ["java"]
processesLimit = 50
requestTimeout = "5s"
scriptTimeout = "1s"
maxBodyBytes = 65536
maxConcurrentRuns = 2
` + files
		if err := os.WriteFile(configPath, []byte(text), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(`
[daemon.files]
enabled = true
secret = "files-secret"
roots = ["` + rootA + `"]
`)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewApp(cfg.Daemon, nil)
	if err != nil {
		t.Fatal(err)
	}
	app.SetConfigPath(configPath)
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	base := "http://" + app.ListenAddr()

	status, _ := fileJSON(t, http.MethodGet, base+"/api/v1/files/list?path="+urlQuery(rootA), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("initial root status = %d", status)
	}

	// The policy is read from the reloadable snapshot per request, so a
	// config change takes effect without a restart.
	writeConfig(`
[daemon.files]
enabled = true
secret = "files-secret"
roots = ["` + rootB + `"]
allowWrite = true
`)
	if err := app.Reload(); err != nil {
		t.Fatal(err)
	}
	status, _ = fileJSON(t, http.MethodGet, base+"/api/v1/files/list?path="+urlQuery(rootA), nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("stale root status = %d", status)
	}
	status, _ = fileJSON(t, http.MethodGet, base+"/api/v1/files/list?path="+urlQuery(rootB), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("new root status = %d", status)
	}

	// Turning the API off in the file turns it off at the next request.
	writeConfig("")
	if err := app.Reload(); err != nil {
		t.Fatal(err)
	}
	status, _ = fileJSON(t, http.MethodGet, base+"/api/v1/files/roots", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("disabled after reload status = %d", status)
	}
}

func TestFileAPIDedicatedSecretOverridesMetricsSecret(t *testing.T) {
	root := t.TempDir()
	_, base := fileDaemon(t, root, func(f *config.FilesConfig) { f.Secret = "only-this" })
	status, _ := fileJSON(t, http.MethodGet, base+"/api/v1/files/roots", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("metrics secret status = %d", status)
	}
	status, _ = fileJSON(t, http.MethodGet, base+"/api/v1/files/roots", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer only-this")
	})
	if status != http.StatusOK {
		t.Fatalf("dedicated secret status = %d", status)
	}
}

func TestFileAPIMkdirAndStatFollow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	_, base := fileDaemon(t, root, nil)

	mkdirBody := []byte(`{"path":` + strconv.Quote(filepath.Join(root, "a", "b")) + `,"parents":true}`)
	status, response := fileJSON(t, http.MethodPost, base+"/api/v1/files/mkdir", mkdirBody, nil)
	if status != http.StatusOK {
		t.Fatalf("mkdir status = %d (%v)", status, response)
	}
	if info, err := os.Stat(filepath.Join(root, "a", "b")); err != nil || !info.IsDir() {
		t.Fatalf("mkdir result: %v", err)
	}
	// A directory that already exists is a conflict, not a silent success.
	status, _ = fileJSON(t, http.MethodPost, base+"/api/v1/files/mkdir", []byte(`{"path":`+
		strconv.Quote(filepath.Join(root, "a"))+`}`), nil)
	if status != http.StatusConflict {
		t.Fatalf("repeat mkdir status = %d", status)
	}

	// stat follows a link by default and reports the target's kind and size.
	status, response = fileJSON(t, http.MethodGet, base+"/api/v1/files/stat?path="+urlQuery(filepath.Join(root, "link")), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("stat status = %d", status)
	}
	if response["type"] != "file" || response["size"].(float64) != 4 {
		t.Fatalf("followed stat = %v", response)
	}
	// A missing path is a 404 wherever it is missing.
	status, _ = fileJSON(t, http.MethodGet, base+"/api/v1/files/stat?path="+urlQuery(filepath.Join(root, "nope")), nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing stat status = %d", status)
	}
}
