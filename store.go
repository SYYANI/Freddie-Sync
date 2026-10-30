package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

var (
	idPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)
	hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	kinds       = map[string]bool{"paper": true, "attachment": true, "folder": true, "folderMembership": true, "itemType": true, "note": true, "readingState": true, "translationSegment": true, "pdfAnnotation": true, "writing": true, "htmlState": true}
)

const maxPayloadBytes = 256 << 10

type Device struct {
	ID        string `json:"id"`
	LibraryID string `json:"libraryID"`
	Label     string `json:"label"`
	CreatedAt string `json:"createdAt"`
	Revoked   bool   `json:"revoked"`
}

type Credential struct {
	BaseURL string `json:"baseURL,omitempty"`
	Device  Device `json:"device"`
	Token   string `json:"token"`
}

type Library struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Default   bool   `json:"default"`
	CreatedAt string `json:"createdAt,omitempty"`
}

type LibrarySummary struct {
	Library
	BlobBytes     uint64          `json:"blobBytes"`
	BlobCount     uint64          `json:"blobCount"`
	MetadataBytes uint64          `json:"metadataBytes"`
	Records       int             `json:"records"`
	Revision      uint64          `json:"revision"`
	Devices       []DeviceSummary `json:"devices"`
}

type DeviceSummary struct {
	Device
	LastSeenAt string `json:"lastSeenAt,omitempty"`
}

type Record struct {
	Kind      string          `json:"kind"`
	ID        string          `json:"id"`
	Revision  uint64          `json:"revision"`
	Deleted   bool            `json:"deleted"`
	Payload   json.RawMessage `json:"payload"`
	Blobs     []string        `json:"blobs"`
	DeviceID  string          `json:"deviceID"`
	UpdatedAt string          `json:"updatedAt"`
}

type Mutation struct {
	Kind         string          `json:"kind"`
	ID           string          `json:"id"`
	BaseRevision uint64          `json:"baseRevision"`
	Deleted      bool            `json:"deleted"`
	Restore      bool            `json:"restore,omitempty"`
	Payload      json.RawMessage `json:"payload"`
	Blobs        []string        `json:"blobs"`
}

type Push struct {
	RequestID string     `json:"requestID"`
	Mutations []Mutation `json:"mutations"`
}

type Receipt struct {
	RequestID string   `json:"requestID"`
	Revisions []uint64 `json:"revisions"`
}

type storedReceipt struct {
	Hash    string  `json:"hash"`
	Receipt Receipt `json:"receipt"`
}

type Page struct {
	Changes      []Record `json:"changes"`
	Cursor       uint64   `json:"cursor"`
	HeadRevision uint64   `json:"headRevision"`
	HasMore      bool     `json:"hasMore"`
}

type apiError struct {
	Status  int     `json:"-"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Current *Record `json:"current,omitempty"`
}

func (e *apiError) Error() string { return e.Code }
func problem(status int, code, message string) error {
	return &apiError{Status: status, Code: code, Message: message}
}

type Store struct {
	db               *bolt.DB
	root             string
	maxBlobBytes     uint64
	maxMetadataBytes uint64
}

func openStore(root string, blobQuota, metadataQuota uint64) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "blobs"), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "staging"), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(root, "sync.db"), 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, root: root, maxBlobBytes: blobQuota, maxMetadataBytes: metadataQuota}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"meta", "devices", "tokens", "libraries"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		meta := tx.Bucket([]byte("meta"))
		if version := meta.Get([]byte("schema")); version != nil && string(version) != "1" {
			return errors.New("unsupported database schema")
		}
		if err := meta.Put([]byte("schema"), []byte("1")); err != nil {
			return err
		}
		if meta.Get([]byte("storeID")) == nil {
			return meta.Put([]byte("storeID"), []byte(randomID()))
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	// A crash can leave an incomplete upload; it can never be referenced by a record.
	entries, err := os.ReadDir(filepath.Join(root, "staging"))
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			if err := os.Remove(filepath.Join(root, "staging", entry.Name())); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	if err := s.recoverBlobStorage(); err != nil {
		db.Close()
		return nil, err
	}
	if err := syncDirectory(root); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// The file is fsynced before its index entry commits. An interrupted commit may
// leave an unindexed immutable file; delete only these known-format orphans.
func (s *Store) recoverBlobStorage() error {
	root := filepath.Join(s.root, "blobs")
	return s.db.View(func(tx *bolt.Tx) error {
		libraries := tx.Bucket([]byte("libraries"))
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			parts := strings.Split(relative, string(os.PathSeparator))
			if len(parts) != 3 || !idPattern.MatchString(parts[0]) || !hashPattern.MatchString(parts[2]) || parts[1] != parts[2][:2] || !entry.Type().IsRegular() {
				return errors.New("unexpected file in blob storage")
			}
			library := libraries.Bucket([]byte(parts[0]))
			if library == nil || library.Bucket([]byte("blobs")).Get([]byte(parts[2])) == nil {
				return os.Remove(path)
			}
			return nil
		}); err != nil {
			return err
		}
		return libraries.ForEach(func(id, _ []byte) error {
			return libraries.Bucket(id).Bucket([]byte("blobs")).ForEach(func(hash, size []byte) error {
				if !idPattern.Match(id) || !hashPattern.Match(hash) {
					return errors.New("invalid blob index")
				}
				stat, err := os.Lstat(s.blobPath(string(id), string(hash)))
				if err != nil {
					return err
				}
				if !stat.Mode().IsRegular() || len(size) != 8 || uint64(stat.Size()) != binary.BigEndian.Uint64(size) {
					return errors.New("indexed blob is missing or damaged; restore a verified backup")
				}
				return nil
			})
		})
	})
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func digest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func encode(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
func sequence(n uint64) []byte { var b [8]byte; binary.BigEndian.PutUint64(b[:], n); return b[:] }
func counter(b *bolt.Bucket, key string) uint64 {
	data := b.Get([]byte(key))
	if len(data) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(data)
}

var libraryBuckets = []string{"records", "changes", "requests", "blobs", "meta"}

func validName(name, field string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) < 1 || len(name) > 100 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", problem(400, "invalid_"+field, "Name must contain 1–100 bytes without control characters.")
	}
	return name, nil
}

func createLibrary(tx *bolt.Tx, id string) (*bolt.Bucket, error) {
	library, err := tx.Bucket([]byte("libraries")).CreateBucket([]byte(id))
	if err != nil {
		return nil, err
	}
	for _, name := range libraryBuckets {
		if _, err := library.CreateBucket([]byte(name)); err != nil {
			return nil, err
		}
	}
	return library, library.Bucket([]byte("meta")).Put([]byte("createdAt"), []byte(time.Now().UTC().Format(time.RFC3339Nano)))
}

// Libraries are created explicitly, so a mistyped ID cannot start an empty library.
func (s *Store) addLibrary(name, id string) (Library, error) {
	var result Library
	name, err := validName(name, "name")
	if err != nil {
		return result, err
	}
	if id == "" {
		id = randomID()
	}
	if !idPattern.MatchString(id) {
		return result, problem(400, "invalid_library", "Invalid library identifier.")
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("libraries")).Bucket([]byte(id)) != nil {
			return problem(409, "library_exists", "A library with this identifier already exists.")
		}
		library, err := createLibrary(tx, id)
		if err != nil {
			return err
		}
		meta := library.Bucket([]byte("meta"))
		result = Library{ID: id, Name: name, CreatedAt: string(meta.Get([]byte("createdAt")))}
		return meta.Put([]byte("name"), []byte(name))
	})
	return result, err
}

func (s *Store) renameLibrary(id, name string) error {
	name, err := validName(name, "name")
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		library := tx.Bucket([]byte("libraries")).Bucket([]byte(id))
		if library == nil {
			return problem(404, "unknown_library", "Unknown library.")
		}
		return library.Bucket([]byte("meta")).Put([]byte("name"), []byte(name))
	})
}

// Summaries for the local administration page; devices are grouped by library.
func (s *Store) libraries(lastSeen map[string]time.Time) ([]LibrarySummary, error) {
	summaries := []LibrarySummary{}
	err := s.db.View(func(tx *bolt.Tx) error {
		defaultID := string(tx.Bucket([]byte("meta")).Get([]byte("defaultLibrary")))
		index := map[string]int{}
		libraries := tx.Bucket([]byte("libraries"))
		if err := libraries.ForEach(func(id, _ []byte) error {
			library := libraries.Bucket(id)
			meta := library.Bucket([]byte("meta"))
			index[string(id)] = len(summaries)
			summaries = append(summaries, LibrarySummary{
				Library:       Library{ID: string(id), Name: string(meta.Get([]byte("name"))), Default: string(id) == defaultID, CreatedAt: string(meta.Get([]byte("createdAt")))},
				BlobBytes:     counter(meta, "blobBytes"),
				BlobCount:     counter(meta, "blobCount"),
				MetadataBytes: counter(meta, "metadataBytes"),
				Records:       library.Bucket([]byte("records")).Stats().KeyN,
				Revision:      library.Bucket([]byte("changes")).Sequence(),
				Devices:       []DeviceSummary{},
			})
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket([]byte("devices")).ForEach(func(_, v []byte) error {
			var d Device
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			i, ok := index[d.LibraryID]
			if !ok {
				return errors.New("device refers to a missing library")
			}
			summary := DeviceSummary{Device: d}
			if seen, ok := lastSeen[d.ID]; ok {
				summary.LastSeenAt = seen.UTC().Format(time.RFC3339)
			}
			summaries[i].Devices = append(summaries[i].Devices, summary)
			return nil
		})
	})
	for _, summary := range summaries {
		sort.Slice(summary.Devices, func(i, j int) bool { return summary.Devices[i].CreatedAt < summary.Devices[j].CreatedAt })
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		a, b := summaries[i], summaries[j]
		if a.Default != b.Default {
			return a.Default
		}
		return a.CreatedAt < b.CreatedAt
	})
	return summaries, err
}

// Deletes a library no device can use any more: its records, history, files, and its
// revoked devices. The default library is never deleted. Called under the server's
// upload mutex; a file left behind by a failed removal is an orphan that
// recoverBlobStorage deletes at the next start.
func (s *Store) deleteLibrary(id string) error {
	if !idPattern.MatchString(id) {
		return problem(400, "invalid_library", "Invalid library identifier.")
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		libraries := tx.Bucket([]byte("libraries"))
		if libraries.Bucket([]byte(id)) == nil {
			return problem(404, "unknown_library", "Unknown library.")
		}
		if string(tx.Bucket([]byte("meta")).Get([]byte("defaultLibrary"))) == id {
			return problem(409, "default_library", "The default library cannot be deleted.")
		}
		devices := tx.Bucket([]byte("devices"))
		owned := map[string]bool{}
		if err := devices.ForEach(func(key, v []byte) error {
			var d Device
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			if d.LibraryID != id {
				return nil
			}
			if !d.Revoked {
				return problem(409, "library_in_use", "Revoke every device of this library first.")
			}
			owned[d.ID] = true
			return nil
		}); err != nil {
			return err
		}
		tokens := tx.Bucket([]byte("tokens"))
		var stale [][]byte
		if err := tokens.ForEach(func(digest, deviceID []byte) error {
			if owned[string(deviceID)] {
				stale = append(stale, append([]byte(nil), digest...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, digest := range stale {
			if err := tokens.Delete(digest); err != nil {
				return err
			}
		}
		for deviceID := range owned {
			if err := devices.Delete([]byte(deviceID)); err != nil {
				return err
			}
		}
		return libraries.DeleteBucket([]byte(id))
	})
	if err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.root, "blobs", id)); err != nil {
		return err
	}
	return syncDirectory(filepath.Join(s.root, "blobs"))
}

func (s *Store) addDevice(label, libraryID string) (Credential, error) {
	var result Credential
	label, err := validName(label, "label")
	if err != nil {
		return result, err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return result, err
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		libraries := tx.Bucket([]byte("libraries"))
		if libraryID == "" {
			libraryID = string(meta.Get([]byte("defaultLibrary")))
			if libraryID == "" {
				libraryID = randomID()
				if err := meta.Put([]byte("defaultLibrary"), []byte(libraryID)); err != nil {
					return err
				}
			}
			if libraries.Bucket([]byte(libraryID)) == nil {
				if _, err := createLibrary(tx, libraryID); err != nil {
					return err
				}
			}
		} else if !idPattern.MatchString(libraryID) {
			return problem(400, "invalid_library", "Invalid library identifier.")
		} else if libraries.Bucket([]byte(libraryID)) == nil {
			return problem(404, "unknown_library", "Unknown library. Create it first.")
		}
		device := Device{ID: randomID(), LibraryID: libraryID, Label: label, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		result = Credential{Device: device, Token: "fs1_" + hex.EncodeToString(secret[:])}
		if err := tx.Bucket([]byte("devices")).Put([]byte(device.ID), encode(device)); err != nil {
			return err
		}
		return tx.Bucket([]byte("tokens")).Put([]byte(digest([]byte(result.Token))), []byte(device.ID))
	})
	return result, err
}

func (s *Store) authenticate(token string) (Device, error) {
	var device Device
	if len(token) != 68 {
		return device, problem(401, "unauthorized", "A valid device token is required.")
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		id := tx.Bucket([]byte("tokens")).Get([]byte(digest([]byte(token))))
		if id == nil {
			return problem(401, "unauthorized", "A valid device token is required.")
		}
		if err := json.Unmarshal(tx.Bucket([]byte("devices")).Get(id), &device); err != nil {
			return err
		}
		if device.Revoked {
			return problem(401, "unauthorized", "A valid device token is required.")
		}
		return nil
	})
	return device, err
}

func activeLibrary(tx *bolt.Tx, device Device) (*bolt.Bucket, error) {
	var current Device
	if err := json.Unmarshal(tx.Bucket([]byte("devices")).Get([]byte(device.ID)), &current); err != nil {
		return nil, err
	}
	if current.Revoked || current.LibraryID != device.LibraryID {
		return nil, problem(401, "unauthorized", "Device has been revoked.")
	}
	return tx.Bucket([]byte("libraries")).Bucket([]byte(device.LibraryID)), nil
}

func (s *Store) devices() ([]Device, error) {
	devices := []Device{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("devices")).ForEach(func(_, v []byte) error {
			var d Device
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			devices = append(devices, d)
			return nil
		})
	})
	return devices, err
}

func (s *Store) revokeDevice(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("devices"))
		data := b.Get([]byte(id))
		if data == nil {
			return problem(404, "not_found", "Unknown device.")
		}
		var d Device
		if err := json.Unmarshal(data, &d); err != nil {
			return err
		}
		d.Revoked = true
		return b.Put([]byte(id), encode(d))
	})
}

func validatePush(push *Push) error {
	if !idPattern.MatchString(push.RequestID) || len(push.Mutations) < 1 || len(push.Mutations) > 100 {
		return problem(400, "invalid_batch", "Provide a requestID and 1–100 mutations.")
	}
	seen := map[string]bool{}
	for i := range push.Mutations {
		m := &push.Mutations[i]
		key := m.Kind + "/" + m.ID
		if !kinds[m.Kind] || !idPattern.MatchString(m.ID) || seen[key] {
			return problem(400, "invalid_record", "Invalid kind, identifier, or duplicate record in batch.")
		}
		seen[key] = true
		if len(m.Payload) > maxPayloadBytes || len(m.Blobs) > 256 {
			return problem(413, "record_too_large", "Record exceeds its size limit.")
		}
		if m.Deleted {
			if m.Restore || (len(m.Payload) > 0 && string(m.Payload) != "null") || len(m.Blobs) > 0 {
				return problem(400, "invalid_tombstone", "Deleted records must have null payload and no blobs.")
			}
			m.Payload = json.RawMessage("null")
		} else {
			var value map[string]json.RawMessage
			if err := json.Unmarshal(m.Payload, &value); err != nil || value == nil {
				return problem(400, "invalid_payload", "Record payload must be a JSON object.")
			}
			m.Payload = encode(value)
		}
		if m.Blobs == nil {
			m.Blobs = []string{}
		}
		seenBlobs := map[string]bool{}
		for _, hash := range m.Blobs {
			if !hashPattern.MatchString(hash) || seenBlobs[hash] {
				return problem(400, "invalid_blob", "Invalid or duplicate blob hash.")
			}
			seenBlobs[hash] = true
		}
	}
	return nil
}

func (s *Store) push(device Device, push Push) (Receipt, error) {
	var receipt Receipt
	if err := validatePush(&push); err != nil {
		return receipt, err
	}
	requestHash := digest(encode(push))
	err := s.db.Update(func(tx *bolt.Tx) error {
		lib, err := activeLibrary(tx, device)
		if err != nil {
			return err
		}
		requests := lib.Bucket([]byte("requests"))
		requestKey := []byte(device.ID + "/" + push.RequestID)
		if saved := requests.Get(requestKey); saved != nil {
			var stored storedReceipt
			if err := json.Unmarshal(saved, &stored); err != nil {
				return err
			}
			if stored.Hash != requestHash {
				return problem(409, "request_id_reused", "A requestID cannot be reused for different content.")
			}
			receipt = stored.Receipt
			return nil
		}
		records := lib.Bucket([]byte("records"))
		changes := lib.Bucket([]byte("changes"))
		blobs := lib.Bucket([]byte("blobs"))
		// Validate the entire batch before writing anything. One conflict rejects all changes.
		for _, m := range push.Mutations {
			var current Record
			saved := records.Get([]byte(m.Kind + "/" + m.ID))
			if saved != nil {
				if err := json.Unmarshal(saved, &current); err != nil {
					return err
				}
			}
			if current.Revision != m.BaseRevision || (saved == nil && m.Deleted) || (current.Deleted && !m.Restore) || (m.Restore && !current.Deleted) {
				e := &apiError{Status: 409, Code: "revision_conflict", Message: "Fetch changes and reconcile before retrying with a new requestID."}
				if saved != nil {
					e.Current = &current
				}
				return e
			}
			for _, hash := range m.Blobs {
				if blobs.Get([]byte(hash)) == nil {
					return problem(409, "missing_blob", "Upload referenced blobs before committing records.")
				}
			}
		}
		receipt = Receipt{RequestID: push.RequestID, Revisions: []uint64{}}
		usage := counter(lib.Bucket([]byte("meta")), "metadataBytes")
		for _, m := range push.Mutations {
			revision, err := changes.NextSequence()
			if err != nil {
				return err
			}
			record := Record{Kind: m.Kind, ID: m.ID, Revision: revision, Deleted: m.Deleted, Payload: m.Payload, Blobs: m.Blobs, DeviceID: device.ID, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			key := []byte(m.Kind + "/" + m.ID)
			data := encode(record)
			usage = usage - uint64(len(records.Get(key))) + uint64(len(data)*2+len(key)+8)
			if err := records.Put(key, data); err != nil {
				return err
			}
			if err := changes.Put(sequence(revision), data); err != nil {
				return err
			}
			receipt.Revisions = append(receipt.Revisions, revision)
		}
		saved := encode(storedReceipt{Hash: requestHash, Receipt: receipt})
		usage += uint64(len(saved) + len(requestKey))
		if usage > s.maxMetadataBytes {
			return problem(507, "metadata_quota_exceeded", "Library metadata quota exceeded.")
		}
		if err := lib.Bucket([]byte("meta")).Put([]byte("metadataBytes"), sequence(usage)); err != nil {
			return err
		}
		return requests.Put(requestKey, saved)
	})
	return receipt, err
}

func (s *Store) changes(device Device, after, until uint64, limit int) (Page, error) {
	page := Page{Changes: []Record{}, Cursor: after}
	err := s.db.View(func(tx *bolt.Tx) error {
		lib, err := activeLibrary(tx, device)
		if err != nil {
			return err
		}
		b := lib.Bucket([]byte("changes"))
		page.HeadRevision = b.Sequence()
		end := page.HeadRevision
		if until != 0 && until < end {
			end = until
		}
		if after > end {
			return problem(409, "invalid_cursor", "Cursor exceeds the server history; do not reset it silently.")
		}
		c := b.Cursor()
		size := 0
		for k, v := c.Seek(sequence(after + 1)); k != nil; k, v = c.Next() {
			n := binary.BigEndian.Uint64(k)
			if n > end {
				break
			}
			if len(page.Changes) >= limit || (size+len(v) > 4<<20 && len(page.Changes) > 0) {
				page.HasMore = true
				break
			}
			var record Record
			if err := json.Unmarshal(v, &record); err != nil {
				return err
			}
			page.Changes = append(page.Changes, record)
			page.Cursor = n
			size += len(v)
		}
		return nil
	})
	return page, err
}

func (s *Store) record(device Device, kind, id string) (Record, error) {
	var record Record
	err := s.db.View(func(tx *bolt.Tx) error {
		lib, err := activeLibrary(tx, device)
		if err != nil {
			return err
		}
		data := lib.Bucket([]byte("records")).Get([]byte(kind + "/" + id))
		if data == nil {
			return problem(404, "not_found", "Unknown record.")
		}
		return json.Unmarshal(data, &record)
	})
	return record, err
}

func (s *Store) blobPath(libraryID, hash string) string {
	return filepath.Join(s.root, "blobs", libraryID, hash[:2], hash)
}

func (s *Store) blobSize(device Device, hash string) (int64, error) {
	var size int64
	err := s.db.View(func(tx *bolt.Tx) error {
		lib, err := activeLibrary(tx, device)
		if err != nil {
			return err
		}
		data := lib.Bucket([]byte("blobs")).Get([]byte(hash))
		if len(data) != 8 {
			return problem(404, "not_found", "Unknown blob.")
		}
		size = int64(binary.BigEndian.Uint64(data))
		return nil
	})
	return size, err
}

// Called under the server's upload mutex, so concurrent uploads cannot exceed quota.
func (s *Store) putBlob(device Device, hash string, size int64, reader io.Reader) error {
	if !hashPattern.MatchString(hash) || size < 0 || size > maxBlobSize {
		return problem(400, "invalid_blob", "Invalid blob hash or length.")
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		lib, err := activeLibrary(tx, device)
		if err != nil {
			return err
		}
		if counter(lib.Bucket([]byte("meta")), "blobBytes")+uint64(size) > s.maxBlobBytes {
			return problem(507, "blob_quota_exceeded", "Library file quota exceeded.")
		}
		if counter(lib.Bucket([]byte("meta")), "blobCount") >= 100000 {
			return problem(507, "blob_quota_exceeded", "Library file count quota exceeded.")
		}
		return nil
	})
	if err != nil {
		return err
	}
	var disk unix.Statfs_t
	if err := unix.Statfs(s.root, &disk); err != nil {
		return err
	}
	if uint64(disk.Bavail)*uint64(disk.Bsize) < uint64(size)+(256<<20) {
		return problem(507, "disk_space_low", "Server disk reserve reached.")
	}
	f, err := os.CreateTemp(filepath.Join(s.root, "staging"), "upload-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, h), reader)
	if err != nil {
		return err
	}
	if written != size {
		return problem(400, "size_mismatch", "Content-Length does not match uploaded bytes.")
	}
	if hex.EncodeToString(h.Sum(nil)) != hash {
		return problem(422, "hash_mismatch", "SHA-256 does not match the upload URL.")
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	path := s.blobPath(device.LibraryID, hash)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	for _, directory := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path)), filepath.Join(s.root, "blobs")} {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		lib, err := activeLibrary(tx, device)
		if err != nil {
			return err
		}
		blobs := lib.Bucket([]byte("blobs"))
		meta := lib.Bucket([]byte("meta"))
		if blobs.Get([]byte(hash)) != nil {
			return nil
		}
		usage := counter(meta, "blobBytes") + uint64(size)
		if usage > s.maxBlobBytes {
			return problem(507, "blob_quota_exceeded", "Library file quota exceeded.")
		}
		if err := blobs.Put([]byte(hash), sequence(uint64(size))); err != nil {
			return err
		}
		if err := meta.Put([]byte("blobCount"), sequence(counter(meta, "blobCount")+1)); err != nil {
			return err
		}
		return meta.Put([]byte("blobBytes"), sequence(usage))
	})
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncTreeDirectories(root string) error {
	directories := []string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := syncDirectory(directories[i]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) snapshot(w io.Writer) error {
	return s.db.View(func(tx *bolt.Tx) error { _, err := tx.WriteTo(w); return err })
}

func verifyBackup(root string) error {
	db, err := bolt.Open(filepath.Join(root, "sync.db"), 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return err
	}
	defer db.Close()
	return db.View(func(tx *bolt.Tx) error {
		var checkErr error
		for err := range tx.Check() {
			if err != nil {
				checkErr = err
			}
		}
		if checkErr != nil {
			return checkErr
		}
		if meta := tx.Bucket([]byte("meta")); meta == nil || string(meta.Get([]byte("schema"))) != "1" {
			return errors.New("unsupported backup schema")
		}
		libraries := tx.Bucket([]byte("libraries"))
		if libraries == nil {
			return errors.New("missing libraries")
		}
		return libraries.ForEach(func(id, _ []byte) error {
			lib := libraries.Bucket(id)
			if lib == nil || !idPattern.Match(id) {
				return errors.New("invalid library bucket")
			}
			for _, name := range []string{"records", "changes", "requests", "blobs", "meta"} {
				if lib.Bucket([]byte(name)) == nil {
					return errors.New("incomplete library schema")
				}
			}
			return lib.Bucket([]byte("blobs")).ForEach(func(hash, size []byte) error {
				if !hashPattern.Match(hash) || len(size) != 8 {
					return errors.New("invalid blob index")
				}
				path := filepath.Join(root, "blobs", string(id), string(hash[:2]), string(hash))
				stat, err := os.Lstat(path)
				if err != nil {
					return err
				}
				if !stat.Mode().IsRegular() {
					return errors.New("backup contains a non-regular blob")
				}
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				h := sha256.New()
				n, err := io.Copy(h, f)
				if err != nil {
					return err
				}
				if uint64(n) != binary.BigEndian.Uint64(size) || !bytes.Equal([]byte(hex.EncodeToString(h.Sum(nil))), hash) {
					return errors.New("backup blob failed integrity verification")
				}
				return nil
			})
		})
	})
}
