package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

const maxBlobSize int64 = 512 << 20

//go:embed admin
var adminAssets embed.FS

type deviceContextKey struct{}
type allowance struct {
	tokens  float64
	updated time.Time
}
type API struct {
	store     *Store
	host      string
	publicURL string
	upload    sync.Mutex
	slots     chan struct{}
	rateMu    sync.Mutex
	rates     map[string]allowance
	seenMu    sync.Mutex
	lastSeen  map[string]time.Time // Since the service started; never written to disk.
}

func newAPI(store *Store, host string) *API {
	return &API{store: store, host: host, publicURL: "https://" + host + "/freddie-sync/", slots: make(chan struct{}, 8), rates: map[string]allowance{}, lastSeen: map[string]time.Time{}}
}

func (a *API) markSeen(deviceID string) {
	a.seenMu.Lock()
	a.lastSeen[deviceID] = time.Now()
	a.seenMu.Unlock()
}

func (a *API) seenSnapshot() map[string]time.Time {
	a.seenMu.Lock()
	defer a.seenMu.Unlock()
	copied := make(map[string]time.Time, len(a.lastSeen))
	for id, seen := range a.lastSeen {
		copied[id] = seen
	}
	return copied
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	var api *apiError
	if errors.As(err, &api) {
		writeJSON(w, api.Status, map[string]any{"error": api})
		return
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSON(w, 413, map[string]any{"error": map[string]string{"code": "request_too_large", "message": "Request exceeds its size limit."}})
		return
	}
	// Do not log request URLs, authorization, payloads, or internal file paths.
	slog.Error("request failed", "type", "internal_error")
	writeJSON(w, 500, map[string]any{"error": map[string]string{"code": "internal_error", "message": "The server could not complete the operation."}})
}

func decodeBody(w http.ResponseWriter, r *http.Request, max int64, target any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return problem(415, "json_required", "Use Content-Type: application/json.")
	}
	r.Body = http.MaxBytesReader(w, r.Body, max)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return problem(400, "invalid_json", "Invalid JSON request.")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return problem(400, "invalid_json", "Provide exactly one JSON object.")
	}
	return nil
}

func (a *API) allowed(deviceID string) bool {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	now := time.Now()
	state, ok := a.rates[deviceID]
	if !ok {
		state = allowance{tokens: 60, updated: now}
	}
	state.tokens = min(60, state.tokens+now.Sub(state.updated).Seconds()*20)
	state.updated = now
	ok = state.tokens >= 1
	if ok {
		state.tokens--
	}
	a.rates[deviceID] = state
	// Entries only use authenticated device IDs, never attacker-controlled IPs.
	if len(a.rates) > 256 {
		for id, entry := range a.rates {
			if now.Sub(entry.updated) > time.Hour {
				delete(a.rates, id)
			}
		}
	}
	return ok
}

func (a *API) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.db.View(func(_ *bolt.Tx) error { return nil }); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/info", a.info)
	mux.HandleFunc("GET /v1/changes", a.getChanges)
	mux.HandleFunc("POST /v1/changes", a.pushChanges)
	mux.HandleFunc("GET /v1/records/{kind}/{id}", a.getRecord)
	mux.HandleFunc("PUT /v1/blobs/{hash}", a.putBlob)
	mux.HandleFunc("GET /v1/blobs/{hash}", a.getBlob)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != a.host && host != "127.0.0.1" && host != "localhost" {
			writeError(w, problem(421, "invalid_host", "Unexpected host."))
			return
		}
		if r.Header.Get("Origin") != "" {
			writeError(w, problem(403, "browser_request_denied", "Use a native sync client."))
			return
		}
		if r.URL.RawQuery != "" && (r.URL.Path == "/v1/info" || r.URL.Path == "/health") {
			writeError(w, problem(400, "invalid_query", "Unexpected query parameters."))
			return
		}
		select {
		case a.slots <- struct{}{}:
			defer func() { <-a.slots }()
		default:
			w.Header().Set("Retry-After", "2")
			writeError(w, problem(503, "busy", "Retry later."))
			return
		}
		if r.URL.Path != "/health" {
			auth := r.Header.Values("Authorization")
			if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, problem(401, "unauthorized", "A device token is required."))
				return
			}
			device, err := a.store.authenticate(strings.TrimPrefix(auth[0], "Bearer "))
			if err != nil {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, err)
				return
			}
			if !a.allowed(device.ID) {
				w.Header().Set("Retry-After", "3")
				writeError(w, problem(429, "rate_limited", "Retry later."))
				return
			}
			a.markSeen(device.ID)
			r = r.WithContext(context.WithValue(r.Context(), deviceContextKey{}, device))
		}
		mux.ServeHTTP(w, r)
	})
}

func requestDevice(r *http.Request) Device { return r.Context().Value(deviceContextKey{}).(Device) }

func (a *API) info(w http.ResponseWriter, r *http.Request) {
	d := requestDevice(r)
	var storeID string
	err := a.store.db.View(func(tx *bolt.Tx) error {
		storeID = string(tx.Bucket([]byte("meta")).Get([]byte("storeID")))
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"protocolVersion": 1, "storeID": storeID, "libraryID": d.LibraryID, "deviceID": d.ID, "limits": map[string]any{"maxBlobBytes": maxBlobSize, "maxPayloadBytes": maxPayloadBytes, "maxMutations": 100}})
}

func (a *API) getChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for key, values := range q {
		if (key != "after" && key != "until" && key != "limit") || len(values) != 1 {
			writeError(w, problem(400, "invalid_query", "Unexpected query parameter."))
			return
		}
	}
	parse := func(name string, defaultValue uint64) (uint64, error) {
		if !q.Has(name) {
			return defaultValue, nil
		}
		n, err := strconv.ParseUint(q.Get(name), 10, 53)
		if err != nil {
			return 0, problem(400, "invalid_query", "Invalid cursor or limit.")
		}
		return n, nil
	}
	after, err := parse("after", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	until, err := parse("until", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	limit, err := parse("limit", 100)
	if err != nil || limit < 1 || limit > 500 {
		writeError(w, problem(400, "invalid_limit", "Limit must be between 1 and 500."))
		return
	}
	page, err := a.store.changes(requestDevice(r), after, until, int(limit))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, page)
}

func (a *API) pushChanges(w http.ResponseWriter, r *http.Request) {
	var push Push
	if err := decodeBody(w, r, 4<<20, &push); err != nil {
		writeError(w, err)
		return
	}
	receipt, err := a.store.push(requestDevice(r), push)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, receipt)
}

func (a *API) getRecord(w http.ResponseWriter, r *http.Request) {
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if !kinds[kind] || !idPattern.MatchString(id) {
		writeError(w, problem(400, "invalid_record", "Invalid record identifier."))
		return
	}
	record, err := a.store.record(requestDevice(r), kind, id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, record)
}

func (a *API) putBlob(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !hashPattern.MatchString(hash) {
		writeError(w, problem(400, "invalid_hash", "Expected a lowercase SHA-256 hash."))
		return
	}
	if r.ContentLength < 0 {
		writeError(w, problem(411, "length_required", "Provide Content-Length for file uploads."))
		return
	}
	if r.ContentLength > maxBlobSize {
		writeError(w, problem(413, "blob_too_large", "File exceeds 512 MiB."))
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/octet-stream" {
		writeError(w, problem(415, "binary_required", "Use application/octet-stream."))
		return
	}
	// Only one upload streams to disk at a time on the small personal server.
	if !a.upload.TryLock() {
		w.Header().Set("Retry-After", "2")
		writeError(w, problem(429, "upload_busy", "Retry this upload later."))
		return
	}
	defer a.upload.Unlock()
	device := requestDevice(r)
	if size, err := a.store.blobSize(device, hash); err == nil {
		if size != r.ContentLength {
			writeError(w, problem(409, "size_mismatch", "Existing blob has a different size."))
			return
		}
		writeJSON(w, 200, map[string]any{"hash": hash, "size": size, "exists": true})
		return
	} else {
		var p *apiError
		if !errors.As(err, &p) || p.Status != 404 {
			writeError(w, err)
			return
		}
	}
	if err := a.store.putBlob(device, hash, r.ContentLength, http.MaxBytesReader(w, r.Body, maxBlobSize)); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"hash": hash, "size": r.ContentLength, "exists": false})
}

func (a *API) getBlob(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !hashPattern.MatchString(hash) {
		writeError(w, problem(400, "invalid_hash", "Expected a lowercase SHA-256 hash."))
		return
	}
	device := requestDevice(r)
	size, err := a.store.blobSize(device, hash)
	if err != nil {
		writeError(w, err)
		return
	}
	f, err := os.Open(a.store.blobPath(device.LibraryID, hash))
	if err != nil {
		writeError(w, err)
		return
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || stat.Size() != size {
		writeError(w, errors.New("blob integrity error"))
		return
	}
	decoded, _ := hex.DecodeString(hash)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="blob"`)
	w.Header().Set("ETag", `"`+hash+`"`)
	w.Header().Set("Repr-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(decoded)+":")
	http.ServeContent(w, r, "blob", time.Time{}, f)
}

// The admin API listens only on a 0600 Unix socket. The page reaches it through an
// SSH forward of that socket, so a browser may be talking to it: accept only
// loopback Host names (against DNS rebinding) and same-origin browser requests.
func adminRequestAllowed(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

func (a *API) adminHandler() http.Handler {
	mux := http.NewServeMux()
	page := func(name, contentType string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			data, err := adminAssets.ReadFile("admin/" + name)
			if err != nil {
				writeError(w, err)
				return
			}
			w.Header().Set("Content-Type", contentType)
			_, _ = w.Write(data)
		}
	}
	mux.HandleFunc("GET /{$}", page("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /admin.css", page("admin.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET /admin.js", page("admin.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /libraries", func(w http.ResponseWriter, r *http.Request) {
		libraries, err := a.store.libraries(a.seenSnapshot())
		if err != nil {
			writeError(w, err)
			return
		}
		var disk unix.Statfs_t
		var free uint64
		if unix.Statfs(a.store.root, &disk) == nil {
			free = uint64(disk.Bavail) * uint64(disk.Bsize)
		}
		writeJSON(w, 200, map[string]any{
			"server":    map[string]any{"version": buildVersion, "publicURL": a.publicURL, "diskFreeBytes": free, "blobQuotaBytes": a.store.maxBlobBytes, "metadataQuotaBytes": a.store.maxMetadataBytes, "maxBlobCount": 100000},
			"libraries": libraries,
		})
	})
	mux.HandleFunc("POST /libraries", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
			ID   string `json:"id"`
		}
		if err := decodeBody(w, r, 4096, &body); err != nil {
			writeError(w, err)
			return
		}
		library, err := a.store.addLibrary(body.Name, body.ID)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 201, library)
	})
	mux.HandleFunc("PATCH /libraries/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := decodeBody(w, r, 4096, &body); err != nil {
			writeError(w, err)
			return
		}
		if err := a.store.renameLibrary(r.PathValue("id"), body.Name); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /libraries/{id}", func(w http.ResponseWriter, r *http.Request) {
		// No upload may reach the library while its files are removed.
		a.upload.Lock()
		defer a.upload.Unlock()
		if err := a.store.deleteLibrary(r.PathValue("id")); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /devices", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Label     string `json:"label"`
			LibraryID string `json:"libraryID"`
		}
		if err := decodeBody(w, r, 4096, &body); err != nil {
			writeError(w, err)
			return
		}
		credential, err := a.store.addDevice(body.Label, body.LibraryID)
		if err != nil {
			writeError(w, err)
			return
		}
		credential.BaseURL = a.publicURL
		writeJSON(w, 201, credential)
	})
	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		devices, err := a.store.devices()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, devices)
	})
	mux.HandleFunc("DELETE /devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.revokeDevice(r.PathValue("id")); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /backup", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if err := a.store.snapshot(w); err != nil {
			slog.Error("backup stream failed")
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		if !adminRequestAllowed(r) {
			writeError(w, problem(403, "admin_request_denied", "Open the administration page through the SSH-forwarded loopback address."))
			return
		}
		mux.ServeHTTP(w, r)
	})
}
