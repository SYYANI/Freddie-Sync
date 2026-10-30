package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	bolt "go.etcd.io/bbolt"
)

var buildVersion = "dev"

const defaultSocket = "/run/freddie-sync/admin.sock"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("command failed", "error", err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: freddie-sync serve|health|library-add|library-list|library-delete|device-add|device-list|device-revoke|backup|verify|restore|version")
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	socket := fs.String("socket", defaultSocket, "Local administration Unix socket")
	switch command {
	case "version":
		fmt.Printf("%s (%s)\n", buildVersion, runtime.Version())
		return nil
	case "serve":
		data := fs.String("data", "/var/lib/freddie-sync", "Private data directory")
		listen := fs.String("listen", "127.0.0.1:17483", "HTTP address behind TLS proxy")
		allowNonLoopback := fs.Bool("allow-non-loopback", false, "Allow non-loopback HTTP inside an isolated container network")
		host := fs.String("host", "", "Public hostname clients use to reach this server (required)")
		publicURL := fs.String("public-url", "", "Sync URL written into new credentials (default https://HOST/freddie-sync/)")
		blobQuota := fs.Uint64("blob-quota", 10<<30, "Per-library immutable file quota, bytes")
		metadataQuota := fs.Uint64("metadata-quota", 512<<20, "Per-library metadata/history quota, bytes")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if !validHostname(*host) {
			return errors.New("serve requires --host, the public hostname of this server, for example sync.example.com")
		}
		if os.Geteuid() == 0 {
			return errors.New("run the service as a dedicated unprivileged user")
		}
		if err := checkListenAddress(*listen, *allowNonLoopback); err != nil {
			return err
		}
		store, err := openStore(*data, *blobQuota, *metadataQuota)
		if err != nil {
			return err
		}
		defer store.db.Close()
		if *publicURL != "" && !strings.HasPrefix(*publicURL, "https://") {
			return errors.New("public URL must use HTTPS")
		}
		return serve(store, *listen, *socket, *host, *publicURL)
	case "health":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return adminCall(*socket, "GET", "/health", nil, io.Discard)
	case "library-add":
		name := fs.String("name", "", "Library name, for example the person it belongs to")
		id := fs.String("id", "", "Library ID; empty generates one")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return adminCall(*socket, "POST", "/libraries", map[string]string{"name": *name, "id": *id}, os.Stdout)
	case "library-list":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return adminCall(*socket, "GET", "/libraries", nil, os.Stdout)
	case "library-delete":
		id := fs.String("id", "", "Library ID; every device must be revoked")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if !idPattern.MatchString(*id) {
			return errors.New("provide a library ID")
		}
		return adminCall(*socket, "DELETE", "/libraries/"+*id, nil, os.Stdout)
	case "device-add":
		label := fs.String("label", "", "Device name")
		library := fs.String("library", "", "Existing library ID; empty selects the personal library")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return adminCall(*socket, "POST", "/devices", map[string]string{"label": *label, "libraryID": *library}, os.Stdout)
	case "device-list":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		var listing bytes.Buffer
		if err := adminCall(*socket, "GET", "/libraries", nil, &listing); err != nil {
			return err
		}
		return printDevices(listing.Bytes(), os.Stdout)
	case "device-revoke":
		id := fs.String("id", "", "Device UUID to revoke")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if !idPattern.MatchString(*id) {
			return errors.New("provide a device ID")
		}
		return adminCall(*socket, "DELETE", "/devices/"+*id, nil, os.Stdout)
	case "backup":
		data := fs.String("data", "/var/lib/freddie-sync", "Live data directory")
		out := fs.String("out", "", "New backup directory (must not exist)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *out == "" {
			return errors.New("provide --out")
		}
		return backup(*socket, *data, *out)
	case "restore":
		from := fs.String("from", "", "Verified backup directory")
		data := fs.String("data", "", "New data directory; must not exist")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *from == "" || *data == "" {
			return errors.New("provide --from and --data")
		}
		return restoreBackup(*from, *data)
	case "verify":
		data := fs.String("data", "", "Backup directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *data == "" {
			return errors.New("provide --data")
		}
		if err := verifyBackup(*data); err != nil {
			return err
		}
		fmt.Println("Backup database and all indexed blobs verified.")
		return nil
	default:
		return errors.New("unknown command")
	}
}

// Restoring is deliberately offline, into a new directory, with a fresh storeID.
// Clients detect the history replacement rather than trusting reused revisions.
func restoreBackup(from, destination string) error {
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return errors.New("restore destination must not exist")
	}
	if err := verifyBackup(from); err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	temp, err := os.MkdirTemp(parent, ".restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	err = filepath.WalkDir(from, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if relative != "sync.db" && relative != "blobs" && !strings.HasPrefix(relative, "blobs"+string(os.PathSeparator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(temp, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("unexpected file in backup")
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if err == nil {
			err = out.Sync()
		}
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	db, err := bolt.Open(filepath.Join(temp, "sync.db"), 0600, nil)
	if err != nil {
		return err
	}
	err = db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Put([]byte("storeID"), []byte(randomID())) })
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := verifyBackup(temp); err != nil {
		return err
	}
	if err := syncTreeDirectories(temp); err != nil {
		return err
	}
	if err := os.Rename(temp, destination); err != nil {
		return err
	}
	return syncDirectory(parent)
}

// Prints devices grouped by library, for reading over SSH.
func printDevices(listing []byte, out io.Writer) error {
	var overview struct {
		Libraries []LibrarySummary `json:"libraries"`
	}
	if err := json.Unmarshal(listing, &overview); err != nil {
		return err
	}
	for i, library := range overview.Libraries {
		if i > 0 {
			fmt.Fprintln(out)
		}
		name := library.Name
		if name == "" {
			name = "(unnamed)"
		}
		if library.Default {
			name += " [default]"
		}
		fmt.Fprintf(out, "%s  %s\n", name, library.ID)
		if len(library.Devices) == 0 {
			fmt.Fprintln(out, "  (no devices)")
		}
		for _, device := range library.Devices {
			status := "active "
			if device.Revoked {
				status = "revoked"
			}
			fmt.Fprintf(out, "  %s  %s  %s  %s\n", status, device.ID, strings.SplitN(device.CreatedAt, "T", 2)[0], device.Label)
		}
	}
	return nil
}

func serve(store *Store, address, socket, host, publicURL string) error {
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("admin socket path contains a non-socket file")
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	adminListener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer adminListener.Close()
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	api := newAPI(store, host)
	if publicURL != "" {
		api.publicURL = publicURL
	}
	public := &http.Server{Handler: api.handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Minute, WriteTimeout: 10 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	admin := &http.Server{Handler: api.adminHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Minute, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- public.Serve(listener) }()
	go func() { errorsChannel <- admin.Serve(adminListener) }()
	slog.Info("service ready", "version", buildVersion, "listen", address)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errorsChannel:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Finish in-flight durable writes before closing the database.
	if err := public.Shutdown(shutdown); err != nil {
		public.Close()
	}
	if err := admin.Shutdown(shutdown); err != nil {
		admin.Close()
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

func adminCall(socket, method, path string, body any, out io.Writer) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(encode(body))
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Minute}
	req, err := http.NewRequest(method, "http://localhost"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("admin request failed (%d): %s", resp.StatusCode, data)
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

func backup(socket, data, out string) error {
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		return errors.New("backup destination must not exist")
	}
	parent := filepath.Dir(out)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(parent, ".partial-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	f, err := os.OpenFile(filepath.Join(temp, "sync.db"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = adminCall(socket, "GET", "/backup", nil, f)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Files are immutable and never garbage-collected in protocol v1. Copy after the
	// database snapshot, so every referenced blob is included even during live writes.
	source := filepath.Join(data, "blobs")
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(temp, "blobs", relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected symlink in blob storage")
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("unexpected non-regular blob file")
		}
		if err := os.Link(path, target); err == nil {
			return nil
		} else if !errors.Is(err, syscall.EXDEV) {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if err == nil {
			err = out.Sync()
		}
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	if err := verifyBackup(temp); err != nil {
		return err
	}
	manifest := map[string]any{"formatVersion": 1, "createdAt": time.Now().UTC().Format(time.RFC3339Nano), "serverVersion": buildVersion, "verified": true, "immutableBlobs": true}
	if err := os.WriteFile(filepath.Join(temp, "backup.json"), append(encode(manifest), '\n'), 0600); err != nil {
		return err
	}
	if err := syncTreeDirectories(temp); err != nil {
		return err
	}
	if err := os.Rename(temp, out); err != nil {
		return err
	}
	if err := syncDirectory(parent); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "verified", "backup": strings.TrimSuffix(out, "/")})
}

// checkListenAddress keeps the HTTP listener on loopback unless a container
// network, whose host-side publication is loopback-only, explicitly opts out.
func checkListenAddress(listen string, allowNonLoopback bool) error {
	h, _, err := net.SplitHostPort(listen)
	ip := net.ParseIP(h)
	if err != nil || ip == nil {
		return errors.New("HTTP listener must use a literal IP address and port")
	}
	if !ip.IsLoopback() && !allowNonLoopback {
		return errors.New("HTTP listener must use a loopback IP; isolated container networks can opt in with --allow-non-loopback")
	}
	return nil
}

// validHostname accepts a bare DNS name: no scheme, port, path or trailing dot.
func validHostname(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
