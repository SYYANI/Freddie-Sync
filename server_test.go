package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func fixture(t *testing.T) (*Store, Credential, *API) {
	t.Helper()
	s, err := openStore(t.TempDir(), 1<<30, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	c, err := s.addDevice("Mac", "")
	if err != nil {
		t.Fatal(err)
	}
	return s, c, newAPI(s, "sync.example")
}

func mutation(id string, base uint64) Mutation {
	return Mutation{Kind: "paper", ID: id, BaseRevision: base, Payload: json.RawMessage(`{"title":"Paper"}`)}
}
func commit(t *testing.T, s *Store, c Credential, ms ...Mutation) Receipt {
	t.Helper()
	receipt, err := s.push(c.Device, Push{RequestID: randomID(), Mutations: ms})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}
func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	e, ok := err.(*apiError)
	if !ok || e.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func request(api *API, token, method, path string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://sync.example"+path, bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(path, "/v1/blobs/") {
		r.Header.Set("Content-Type", "application/octet-stream")
	}
	w := httptest.NewRecorder()
	api.handler().ServeHTTP(w, r)
	return w
}

func TestAuthenticationAndBrowserIsolation(t *testing.T) {
	_, c, a := fixture(t)
	for _, tc := range []struct {
		name, token string
		status      int
	}{{"missing", "", 401}, {"invalid", strings.Repeat("x", 68), 401}, {"valid", c.Token, 200}} {
		t.Run(tc.name, func(t *testing.T) {
			if w := request(a, tc.token, "GET", "/v1/info", nil); w.Code != tc.status {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
	for _, tc := range []struct {
		name, host, origin string
		status             int
	}{{"host", "evil.example", "", 421}, {"browser", "sync.example", "https://evil.example", 403}, {"null-origin", "sync.example", "null", 403}} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://sync.example/v1/info", nil)
			r.Host = tc.host
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Authorization", "Bearer "+c.Token)
			w := httptest.NewRecorder()
			a.handler().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatal(w.Code)
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("CORS enabled")
			}
		})
	}
	if w := request(a, "", "OPTIONS", "/v1/changes", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(a, c.Token, "GET", "/devices", nil); w.Code != 404 {
		t.Fatalf("public admin route: %d", w.Code)
	}
	if w := request(a, "", "GET", "/health", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestAtomicConflictsIdempotencyAndDeletion(t *testing.T) {
	s, c, _ := fixture(t)
	batch := Push{RequestID: randomID(), Mutations: []Mutation{mutation("paper-a", 0), mutation("paper-b", 0)}}
	first, err := s.push(c.Device, batch)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.push(c.Device, batch)
	if err != nil || !bytes.Equal(encode(first), encode(second)) {
		t.Fatalf("retry: %v", err)
	}
	batch.Mutations[0].Payload = json.RawMessage(`{"title":"different"}`)
	_, err = s.push(c.Device, batch)
	expectCode(t, err, "request_id_reused")
	_, err = s.push(c.Device, Push{RequestID: randomID(), Mutations: []Mutation{mutation("paper-c", 0), mutation("paper-a", 0)}})
	expectCode(t, err, "revision_conflict")
	_, err = s.record(c.Device, "paper", "paper-c")
	expectCode(t, err, "not_found")
	page, err := s.changes(c.Device, 0, 0, 100)
	if err != nil || len(page.Changes) != 2 {
		t.Fatalf("atomicity: %v %+v", err, page)
	}
	del := Mutation{Kind: "paper", ID: "paper-a", BaseRevision: first.Revisions[0], Deleted: true}
	deleted := commit(t, s, c, del)
	_, err = s.push(c.Device, Push{RequestID: randomID(), Mutations: []Mutation{mutation("paper-a", deleted.Revisions[0])}})
	expectCode(t, err, "revision_conflict")
	restore := mutation("paper-a", deleted.Revisions[0])
	restore.Restore = true
	commit(t, s, c, restore)
	record, err := s.record(c.Device, "paper", "paper-a")
	if err != nil || record.Deleted {
		t.Fatalf("restore: %v", err)
	}
	// A late retry remains the original receipt even after subsequent edits/deletion.
	batch.Mutations[0] = mutation("paper-a", 0)
	retry, err := s.push(c.Device, batch)
	if err != nil || !bytes.Equal(encode(retry), encode(first)) {
		t.Fatalf("late retry: %v", err)
	}
}

func TestConcurrentWritersCannotOverwriteEachOther(t *testing.T) {
	s, c, _ := fixture(t)
	receipt := commit(t, s, c, mutation("paper", 0))
	var wg sync.WaitGroup
	successes := make(chan struct{}, 20)
	failures := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.push(c.Device, Push{RequestID: randomID(), Mutations: []Mutation{mutation("paper", receipt.Revisions[0])}})
			if err == nil {
				successes <- struct{}{}
			} else {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(successes)
	close(failures)
	if len(successes) != 1 || len(failures) != 19 {
		t.Fatalf("successes %d failures %d", len(successes), len(failures))
	}
	for err := range failures {
		expectCode(t, err, "revision_conflict")
	}
}

func TestChangePaginationHasStableBoundary(t *testing.T) {
	s, c, _ := fixture(t)
	for i := 0; i < 5; i++ {
		commit(t, s, c, mutation(fmt.Sprintf("paper-%d", i), 0))
	}
	first, err := s.changes(c.Device, 0, 0, 2)
	if err != nil || first.Cursor != 2 || first.HeadRevision != 5 || !first.HasMore {
		t.Fatalf("first page: %+v %v", first, err)
	}
	commit(t, s, c, mutation("late-paper", 0))
	second, err := s.changes(c.Device, first.Cursor, first.HeadRevision, 2)
	if err != nil || second.Cursor != 4 || !second.HasMore {
		t.Fatalf("second page: %+v %v", second, err)
	}
	last, err := s.changes(c.Device, second.Cursor, first.HeadRevision, 2)
	if err != nil || last.Cursor != 5 || last.HasMore || len(last.Changes) != 1 {
		t.Fatalf("last page: %+v %v", last, err)
	}
	_, err = s.changes(c.Device, 100, 0, 2)
	expectCode(t, err, "invalid_cursor")
}

func TestLibraryIsolationAndRevocation(t *testing.T) {
	s, c, a := fixture(t)
	library, err := s.addLibrary("Alice", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.addDevice("Other library", library.ID)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, s, c, mutation("private", 0))
	_, err = s.record(other.Device, "paper", "private")
	expectCode(t, err, "not_found")
	data := []byte("private PDF")
	hash := digest(data)
	if err := s.putBlob(c.Device, hash, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if w := request(a, other.Token, "GET", "/v1/blobs/"+hash, nil); w.Code != 404 {
		t.Fatalf("cross-library leak %d", w.Code)
	}
	ipad, err := s.addDevice("iPad", c.Device.LibraryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.record(ipad.Device, "paper", "private"); err != nil {
		t.Fatal(err)
	}
	if err := s.revokeDevice(c.Device.ID); err != nil {
		t.Fatal(err)
	}
	if w := request(a, c.Token, "GET", "/v1/info", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	_, err = s.push(c.Device, Push{RequestID: randomID(), Mutations: []Mutation{mutation("revoked", 0)}})
	expectCode(t, err, "unauthorized")
	if w := request(a, ipad.Token, "GET", "/v1/info", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	// Raw device secrets must never be persisted in the database.
	if err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("tokens")).ForEach(func(k, v []byte) error {
			if bytes.Contains(k, []byte(c.Token)) || bytes.Contains(v, []byte(c.Token)) {
				t.Fatal("plaintext token persisted")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBlobIntegrityRetryRangeAndReference(t *testing.T) {
	s, c, a := fixture(t)
	data := []byte("%PDF-1.7 example\n")
	hash := digest(data)
	if w := request(a, c.Token, "PUT", "/v1/blobs/"+strings.Repeat("0", 64), data); w.Code != 422 {
		t.Fatalf("bad checksum: %d %s", w.Code, w.Body)
	}
	w := request(a, c.Token, "PUT", "/v1/blobs/"+hash, data)
	if w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if w := request(a, c.Token, "PUT", "/v1/blobs/"+hash, data); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request(a, c.Token, "HEAD", "/v1/blobs/"+hash, nil); w.Code != 200 || w.Header().Get("Content-Length") != fmt.Sprint(len(data)) {
		t.Fatalf("HEAD: %d %s", w.Code, w.Header())
	}
	r := httptest.NewRequest("GET", "http://sync.example/v1/blobs/"+hash, nil)
	r.Header.Set("Authorization", "Bearer "+c.Token)
	r.Header.Set("Range", "bytes=0-3")
	w = httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "%PDF" {
		t.Fatalf("range: %d %s", w.Code, w.Body)
	}
	m := mutation("paper", 0)
	m.Blobs = []string{hash}
	commit(t, s, c, m)
	m = mutation("missing", 0)
	m.Blobs = []string{strings.Repeat("a", 64)}
	_, err := s.push(c.Device, Push{RequestID: randomID(), Mutations: []Mutation{m}})
	expectCode(t, err, "missing_blob")
	_, err = s.record(c.Device, "paper", "missing")
	expectCode(t, err, "not_found")
	entries, err := os.ReadDir(filepath.Join(s.root, "staging"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging leak: %v", err)
	}
}

func TestValidationAndQuotas(t *testing.T) {
	s, c, a := fixture(t)
	for _, tc := range []struct {
		name string
		m    Mutation
		code string
	}{
		{"kind", Mutation{Kind: "secret", ID: "x", Payload: json.RawMessage(`{}`)}, "invalid_record"},
		{"path", mutation("../escape", 0), "invalid_record"},
		{"null", Mutation{Kind: "paper", ID: "x", Payload: json.RawMessage(`null`)}, "invalid_payload"},
		{"array", Mutation{Kind: "paper", ID: "x", Payload: json.RawMessage(`[]`)}, "invalid_payload"},
		{"oversize", Mutation{Kind: "paper", ID: "x", Payload: json.RawMessage(`{"x":"` + strings.Repeat("a", maxPayloadBytes) + `"}`)}, "record_too_large"},
		{"delete-payload", Mutation{Kind: "paper", ID: "x", Deleted: true, Payload: json.RawMessage(`{}`)}, "invalid_tombstone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.push(c.Device, Push{RequestID: randomID(), Mutations: []Mutation{tc.m}})
			expectCode(t, err, tc.code)
		})
	}
	for _, path := range []string{"/v1/changes?after=-1", "/v1/changes?limit=501", "/v1/changes?after=0&after=1", "/v1/changes?token=secret"} {
		if w := request(a, c.Token, "GET", path, nil); w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	for _, body := range []string{`{} {}`, `{"requestID":"x","unknown":1}`, `{"requestID":"x","mutations":[]}`} {
		if w := request(a, c.Token, "POST", "/v1/changes", []byte(body)); w.Code != 400 {
			t.Fatalf("bad JSON: %d %s", w.Code, w.Body)
		}
	}
	s.maxBlobBytes = 3
	if err := s.putBlob(c.Device, digest([]byte("four")), 4, strings.NewReader("four")); err == nil {
		t.Fatal("blob quota ignored")
	}
	s.maxMetadataBytes = 1
	_, err := s.push(c.Device, Push{RequestID: randomID(), Mutations: []Mutation{mutation("quota", 0)}})
	expectCode(t, err, "metadata_quota_exceeded")
	page, err := s.changes(c.Device, 0, 0, 100)
	if err != nil || page.HeadRevision != 0 {
		t.Fatalf("quota rollback: %+v %v", page, err)
	}
}

func TestDurableRestartBackupRestoreAndCorruption(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(root, 1<<30, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.addDevice("Mac", "")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("durable attachment")
	hash := digest(data)
	if err := s.putBlob(c.Device, hash, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	m := mutation("durable", 0)
	m.Blobs = []string{hash}
	receipt := commit(t, s, c, m)
	backupRoot := t.TempDir()
	f, err := os.Create(filepath.Join(backupRoot, "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.snapshot(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(backupRoot, "blobs", c.Device.LibraryID, hash[:2], hash)
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackup(backupRoot); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(root, 1<<30, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	if _, err := s.authenticate(c.Token); err != nil {
		t.Fatal(err)
	}
	r, err := s.record(c.Device, "paper", "durable")
	if err != nil || r.Revision != receipt.Revisions[0] {
		t.Fatalf("restart: %v", err)
	}
	restored, err := openStore(backupRoot, 1<<30, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.authenticate(c.Token); err != nil {
		t.Fatal(err)
	}
	commit(t, restored, c, mutation("post-restore", 0))
	if err := restored.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackup(backupRoot); err == nil {
		t.Fatal("corrupt backup accepted")
	}
}

func TestAdminSocketAPIIsSeparate(t *testing.T) {
	s, _, a := fixture(t)
	r := httptest.NewRequest("POST", "http://localhost/devices", strings.NewReader(`{"label":"iPad"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.adminHandler().ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var c Credential
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authenticate(c.Token); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("GET", "http://localhost/backup", nil)
	w = httptest.NewRecorder()
	a.adminHandler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.Len() < 4096 {
		t.Fatal("backup unavailable")
	}
	server := httptest.NewServer(a.handler())
	defer server.Close()
	response, err := http.Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
}

func adminRequest(a *API, method, url, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	a.adminHandler().ServeHTTP(w, r)
	return w
}

func TestLibrariesAreCreatedExplicitlyAndGroupDevices(t *testing.T) {
	s, c, a := fixture(t)
	if _, err := s.addDevice("Typo", "no-such-library"); err == nil {
		t.Fatal("unknown library was created implicitly")
	} else {
		expectCode(t, err, "unknown_library")
	}
	w := adminRequest(a, "POST", "http://localhost/libraries", `{"name":"  Alice  "}`, nil)
	if w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var library Library
	if err := json.Unmarshal(w.Body.Bytes(), &library); err != nil || library.Name != "Alice" || library.ID == "" {
		t.Fatalf("library %+v %v", library, err)
	}
	if w := adminRequest(a, "POST", "http://localhost/libraries", `{"name":"Again","id":"`+library.ID+`"}`, nil); w.Code != 409 {
		t.Fatalf("duplicate library %d", w.Code)
	}
	if w := adminRequest(a, "POST", "http://localhost/libraries", `{"name":"bad\nname"}`, nil); w.Code != 400 {
		t.Fatalf("control character accepted %d", w.Code)
	}
	w = adminRequest(a, "POST", "http://localhost/devices", `{"label":"Alice Mac","libraryID":"`+library.ID+`"}`, nil)
	var alice Credential
	if err := json.Unmarshal(w.Body.Bytes(), &alice); err != nil || w.Code != 201 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if alice.BaseURL != "https://sync.example/freddie-sync/" || alice.Device.LibraryID != library.ID {
		t.Fatalf("credential %+v", alice)
	}
	if w := adminRequest(a, "PATCH", "http://localhost/libraries/"+c.Device.LibraryID, `{"name":"Mine"}`, nil); w.Code != 204 {
		t.Fatalf("rename %d %s", w.Code, w.Body)
	}
	// An authenticated request is shown as recent activity.
	if w := request(a, alice.Token, "GET", "/v1/info", nil); w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = adminRequest(a, "GET", "http://127.0.0.1:17484/libraries", "", nil)
	var overview struct {
		Server    map[string]any   `json:"server"`
		Libraries []LibrarySummary `json:"libraries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &overview); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(overview.Libraries) != 2 || !overview.Libraries[0].Default || overview.Libraries[0].Name != "Mine" || overview.Libraries[1].Name != "Alice" {
		t.Fatalf("libraries %+v", overview.Libraries)
	}
	if devices := overview.Libraries[1].Devices; len(devices) != 1 || devices[0].Label != "Alice Mac" || devices[0].LastSeenAt == "" {
		t.Fatalf("devices %+v", devices)
	}
	if devices := overview.Libraries[0].Devices; len(devices) != 1 || devices[0].LastSeenAt != "" {
		t.Fatalf("devices %+v", devices)
	}
	var printed bytes.Buffer
	if err := printDevices(w.Body.Bytes(), &printed); err != nil || !strings.Contains(printed.String(), "Alice  "+library.ID+"\n  active  ") {
		t.Fatalf("%v %q", err, printed.String())
	}
}

func TestOnlyRetiredLibrariesCanBeDeleted(t *testing.T) {
	s, c, a := fixture(t)
	library, err := s.addLibrary("Smoke test", "smoke-1")
	if err != nil {
		t.Fatal(err)
	}
	device, err := s.addDevice("Integration Mac", library.ID)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("library file")
	hash := digest(data)
	if err := s.putBlob(device.Device, hash, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	commit(t, s, device, Mutation{Kind: "paper", ID: "p", Payload: json.RawMessage(`{"title":"Smoke"}`), Blobs: []string{hash}})
	mine := []byte("my file")
	if err := s.putBlob(c.Device, digest(mine), int64(len(mine)), bytes.NewReader(mine)); err != nil {
		t.Fatal(err)
	}
	expectCode(t, s.deleteLibrary(c.Device.LibraryID), "default_library")
	expectCode(t, s.deleteLibrary(library.ID), "library_in_use")
	expectCode(t, s.deleteLibrary("missing"), "unknown_library")
	if err := s.revokeDevice(device.Device.ID); err != nil {
		t.Fatal(err)
	}
	if w := adminRequest(a, "DELETE", "http://localhost/libraries/"+library.ID, "", nil); w.Code != 204 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(s.root, "blobs", library.ID)); !os.IsNotExist(err) {
		t.Fatalf("library files remain: %v", err)
	}
	devices, err := s.devices()
	if err != nil || len(devices) != 1 || devices[0].ID != c.Device.ID {
		t.Fatalf("devices %+v %v", devices, err)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		if n := tx.Bucket([]byte("tokens")).Stats().KeyN; n != 1 {
			return fmt.Errorf("%d tokens remain", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	libraries, err := s.libraries(nil)
	if err != nil || len(libraries) != 1 || libraries[0].ID != c.Device.LibraryID {
		t.Fatalf("libraries %+v %v", libraries, err)
	}
	// The default library is untouched, and the store and a backup of it still verify.
	if _, err := s.blobSize(c.Device, digest(mine)); err != nil {
		t.Fatal(err)
	}
	backup := t.TempDir()
	f, err := os.Create(filepath.Join(backup, "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.snapshot(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.CopyFS(filepath.Join(backup, "blobs"), os.DirFS(filepath.Join(s.root, "blobs"))); err != nil {
		t.Fatal(err)
	}
	if err := verifyBackup(backup); err != nil {
		t.Fatal(err)
	}
	root := s.root
	s.db.Close()
	reopened, err := openStore(root, 1<<30, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	reopened.db.Close()
}

func TestAdminPageAcceptsOnlySameOriginLoopbackRequests(t *testing.T) {
	_, _, a := fixture(t)
	w := adminRequest(a, "GET", "http://127.0.0.1:17484/", "", map[string]string{"Sec-Fetch-Site": "none"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "admin.js") {
		t.Fatalf("page %d", w.Code)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("csp %q", csp)
	}
	for _, path := range []string{"/admin.js", "/admin.css"} {
		if w := adminRequest(a, "GET", "http://localhost:17484"+path, "", nil); w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("%s %d", path, w.Code)
		}
	}
	allowed := adminRequest(a, "POST", "http://127.0.0.1:17484/libraries", `{"name":"Bob"}`,
		map[string]string{"Origin": "http://127.0.0.1:17484", "Sec-Fetch-Site": "same-origin"})
	if allowed.Code != 201 {
		t.Fatalf("same-origin request %d %s", allowed.Code, allowed.Body)
	}
	denied := []struct {
		url     string
		headers map[string]string
	}{
		{"http://attacker.example:17484/libraries", nil}, // DNS rebinding
		{"http://127.0.0.1:17484/libraries", map[string]string{"Origin": "https://attacker.example"}},
		{"http://127.0.0.1:17484/libraries", map[string]string{"Origin": "http://127.0.0.1:9999"}},
		{"http://127.0.0.1:17484/libraries", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"http://127.0.0.1:17484/libraries", map[string]string{"Sec-Fetch-Site": "same-site"}},
	}
	for _, test := range denied {
		if w := adminRequest(a, "POST", test.url, `{"name":"Mallory"}`, test.headers); w.Code != 403 {
			t.Fatalf("%s %v accepted: %d", test.url, test.headers, w.Code)
		}
	}
	if w := adminRequest(a, "GET", "http://127.0.0.1:17484/backup", "", map[string]string{"Sec-Fetch-Site": "cross-site"}); w.Code != 403 {
		t.Fatalf("cross-site backup %d", w.Code)
	}
}

func TestRealAdminSocketBackupAndSafeRestore(t *testing.T) {
	s, c, a := fixture(t)
	data := []byte("immutable backup data")
	hash := digest(data)
	if err := s.putBlob(c.Device, hash, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	m := mutation("backed-up", 0)
	m.Blobs = []string{hash}
	commit(t, s, c, m)
	socketDirectory, err := os.MkdirTemp("/tmp", "freddie-socket-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDirectory)
	socket := filepath.Join(socketDirectory, "admin.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: a.adminHandler()}
	go server.Serve(listener)
	defer server.Close()
	backupPath := filepath.Join(t.TempDir(), "backup")
	if err := backup(socket, s.root, backupPath); err != nil {
		t.Fatal(err)
	}
	if err := backup(socket, s.root, backupPath); err == nil {
		t.Fatal("existing backup overwritten")
	}
	restoredPath := filepath.Join(t.TempDir(), "restored")
	if err := restoreBackup(backupPath, restoredPath); err != nil {
		t.Fatal(err)
	}
	if err := restoreBackup(backupPath, restoredPath); err == nil {
		t.Fatal("existing data overwritten")
	}
	restored, err := openStore(restoredPath, 1<<30, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.db.Close()
	var oldID, newID string
	s.db.View(func(tx *bolt.Tx) error { oldID = string(tx.Bucket([]byte("meta")).Get([]byte("storeID"))); return nil })
	restored.db.View(func(tx *bolt.Tx) error { newID = string(tx.Bucket([]byte("meta")).Get([]byte("storeID"))); return nil })
	if oldID == newID || newID == "" {
		t.Fatal("restoration did not rotate history identity")
	}
	if _, err := restored.authenticate(c.Token); err != nil {
		t.Fatal(err)
	}
	commit(t, restored, c, mutation("after-restore", 0))
	if err := verifyBackup(backupPath); err != nil {
		t.Fatal(err)
	}
}

func TestCrashRecoveryRemovesUncommittedFiles(t *testing.T) {
	root := t.TempDir()
	s, err := openStore(root, 1<<30, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.addDevice("Mac", "")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("committed")
	hash := digest(data)
	if err := s.putBlob(c.Device, hash, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	orphanHash := digest([]byte("uncommitted"))
	orphan := s.blobPath(c.Device.LibraryID, orphanHash)
	if err := os.MkdirAll(filepath.Dir(orphan), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("uncommitted"), 0600); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(root, "staging", "upload-interrupted")
	if err := os.WriteFile(staging, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(root, 1<<30, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.Close()
	for _, path := range []string{orphan, staging} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("uncommitted file survived recovery")
		}
	}
	if size, err := s.blobSize(c.Device, hash); err != nil || size != int64(len(data)) {
		t.Fatalf("committed file lost: %v", err)
	}
}

func TestServeRequiresPublicHostname(t *testing.T) {
	if err := run([]string{"serve", "--data", t.TempDir()}); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("serve without --host: %v", err)
	}
	for host, valid := range map[string]bool{
		"sync.example.com": true, "localhost": true, "": false, "https://sync.example.com": false,
		"sync.example.com:443": false, "sync.example.com.": false, "-bad.example": false, "a/b": false,
	} {
		if validHostname(host) != valid {
			t.Errorf("validHostname(%q) = %v", host, !valid)
		}
	}
}
