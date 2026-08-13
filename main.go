package main

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/getlantern/systray"
	"github.com/jlaffaye/ftp"
	bbolt "go.etcd.io/bbolt"
	"golang.org/x/crypto/argon2"
)

const defaultRetentionDays = 60
const metadataDirName = ".coldstorage"
const appVersion = "0.1.0"

// retentionDays returns the configured retention period, falling back to the default.
func retentionDuration() time.Duration {
	days := appConfig.RetentionDays
	if days <= 0 {
		days = defaultRetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
}

var appConfig Config
var settingsMode bool
var restorePath string

// Config holds runtime settings for the tray app.
type Config struct {
	Root                   string
	Host                   string
	User                   string
	Pass                   string
	FTPRoot                string
	SyncOnBattery          bool
	PreventSleepDuringSync bool
	ExcludedFolders        []string
	RetentionDays          int
	VerifyUploadSize       bool
	VerifyUploadHash       bool
	HashCommand            string
	AllowUnsafeFreeze      bool
	LastIndexBackup        time.Time
	EncryptionEnabled      bool
	EncryptionSalt         string // base64-encoded 32-byte random salt; passphrase is NEVER stored on disk
	Weekdays               [7]bool
	SyncHour               int
	SyncMinute             int
	Interval               time.Duration
}

// Record tracks a file's lifecycle in local cold storage.
type Record struct {
	LocalPath    string    `json:"local_path"`
	RemotePath   string    `json:"remote_path"`
	UploadedAt   time.Time `json:"uploaded_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Placeholder  bool      `json:"placeholder"`
	ContentHash  string    `json:"content_hash,omitempty"`
	LastModified time.Time `json:"last_modified"`
	FileSize     int64     `json:"file_size,omitempty"`
	Encrypted    bool      `json:"encrypted,omitempty"`
}

// IsExpired returns true when the file should be removed from local disk.
func (r Record) IsExpired(now time.Time) bool {
	return now.After(r.ExpiresAt) || now.Equal(r.ExpiresAt)
}

// State is a bbolt-backed index of all tracked files.
type State struct {
	db *bbolt.DB
}

var recordsBucket = []byte("records")

func newState() *State {
	return &State{}
}

// Open opens (or creates) the bbolt database at the given path.
// If a legacy index.json exists at the same location, its records are migrated.
func (s *State) Open(dbPath string) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return err
	}
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("open state db: %w", err)
	}
	s.db = db

	// Ensure the records bucket exists
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(recordsBucket)
		return err
	}); err != nil {
		return err
	}

	// Migrate from legacy index.json if present and DB is empty
	jsonPath := strings.TrimSuffix(dbPath, ".db") + ".json"
	if err := s.migrateFromJSON(jsonPath); err != nil {
		log.Printf("warning: json migration failed: %v", err)
	}
	return nil
}

func (s *State) migrateFromJSON(jsonPath string) error {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var legacy struct {
		Records map[string]Record `json:"records"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	// Only migrate if DB is currently empty
	count := 0
	s.db.View(func(tx *bbolt.Tx) error {
		count = tx.Bucket(recordsBucket).Stats().KeyN
		return nil
	})
	if count > 0 {
		return nil
	}
	for path, rec := range legacy.Records {
		if err := s.Put(path, rec); err != nil {
			return err
		}
	}
	log.Printf("migrated %d records from index.json to index.db", len(legacy.Records))
	// Rename the old file so it's not re-migrated
	os.Rename(jsonPath, jsonPath+".migrated")
	return nil
}

func (s *State) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *State) Get(path string) (Record, bool) {
	var rec Record
	found := false
	s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(recordsBucket).Get([]byte(path))
		if v == nil {
			return nil
		}
		if err := json.Unmarshal(v, &rec); err == nil {
			found = true
		}
		return nil
	})
	return rec, found
}

func (s *State) Put(path string, rec Record) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		v, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return tx.Bucket(recordsBucket).Put([]byte(path), v)
	})
}

func (s *State) Delete(path string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(recordsBucket).Delete([]byte(path))
	})
}

func (s *State) ForEach(fn func(path string, rec Record) error) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(recordsBucket).ForEach(func(k, v []byte) error {
			var rec Record
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			return fn(string(k), rec)
		})
	})
}

// DBPath returns the path for the bbolt index database.
func dbPath(root string) string {
	return filepath.Join(root, metadataDirName, "index.db")
}

func defaultConfig() Config {
	return Config{
		Root:                   ".",
		Host:                   "",
		User:                   "",
		Pass:                   "",
		FTPRoot:                "/",
		SyncOnBattery:          false,
		PreventSleepDuringSync: false,
		ExcludedFolders:        []string{},
		RetentionDays:          defaultRetentionDays,
		VerifyUploadSize:       true,
		VerifyUploadHash:       false,
		HashCommand:            "",
		AllowUnsafeFreeze:      false,
		Weekdays:               [7]bool{},
		SyncHour:               23,
		SyncMinute:             0,
		Interval:               5 * time.Minute,
	}
}

func configFilePath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = "."
	}
	return filepath.Join(configDir, "coldstore", "settings.json")
}

func readPersistedConfig() (Config, error) {
	return readConfigFromFile(configFilePath())
}

func readConfigFromFile(cfgPath string) (Config, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func saveConfig(cfg Config) error {
	return saveConfigToFile(cfg, configFilePath())
}

func saveConfigToFile(cfg Config, cfgPath string) error {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, data, 0600)
}

func loadConfig() Config {
	cfg := defaultConfig()
	if persisted, err := readPersistedConfig(); err == nil {
		cfg = persisted
	}

	flag.BoolVar(&settingsMode, "settings", false, "open the settings window")
	flag.StringVar(&restorePath, "restore", "", "restore a single .frozen file by path")
	flag.StringVar(&cfg.Root, "root", cfg.Root, "Local folder to sync")
	flag.StringVar(&cfg.Host, "host", cfg.Host, "FTP host")
	flag.StringVar(&cfg.User, "user", cfg.User, "FTP username")
	flag.StringVar(&cfg.Pass, "pass", cfg.Pass, "FTP password")
	flag.StringVar(&cfg.FTPRoot, "ftp-root", cfg.FTPRoot, "FTP root folder")
	flag.BoolVar(&cfg.SyncOnBattery, "sync-on-battery", cfg.SyncOnBattery, "Allow sync while on battery")
	flag.BoolVar(&cfg.PreventSleepDuringSync, "prevent-sleep", cfg.PreventSleepDuringSync, "Prevent machine sleep during sync")
	flag.DurationVar(&cfg.Interval, "interval", cfg.Interval, "Sync interval")
	flag.Parse()

	if v := os.Getenv("COLDSTORE_ROOT"); v != "" {
		cfg.Root = v
	}
	if v := os.Getenv("COLDSTORE_HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("COLDSTORE_USER"); v != "" {
		cfg.User = v
	}
	if v := os.Getenv("COLDSTORE_PASS"); v != "" {
		cfg.Pass = v
	}
	if v := os.Getenv("COLDSTORE_FTP_ROOT"); v != "" {
		cfg.FTPRoot = v
	}
	if v := os.Getenv("COLDSTORE_SYNC_ON_BATTERY"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.SyncOnBattery = b
		}
	}
	if v := os.Getenv("COLDSTORE_PREVENT_SLEEP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.PreventSleepDuringSync = b
		}
	}
	if v := os.Getenv("COLDSTORE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Interval = d
		}
	}
	if len(flag.Args()) > 0 && !strings.HasPrefix(flag.Args()[0], "-") {
		cfg.Root = flag.Args()[0]
	}
	if cfg.Root == "" {
		cfg.Root = defaultConfig().Root
	}
	return cfg
}

// encryptionMagic is the 4-byte header identifying an encrypted Freezer file.
var encryptionMagic = []byte("FRZR")

// activeEncryptionKey holds the in-memory derived key for the current session.
// It is NEVER written to disk. Nil means encryption is locked / passphrase not entered.
var activeEncryptionKey []byte

// deriveKey runs Argon2id on the passphrase and salt to produce a 32-byte AES key.
func deriveKey(passphrase, saltB64 string) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return nil, fmt.Errorf("invalid encryption salt: %w", err)
	}
	// Argon2id: memory=64MB, iterations=3, parallelism=4, keyLen=32
	key := argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 4, 32)
	return key, nil
}

// newEncryptionSalt generates a fresh random 32-byte salt and returns it as base64.
func newEncryptionSalt() (string, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(salt), nil
}

// encryptStream reads plaintext from r, encrypts with AES-256-GCM, and returns the ciphertext.
// Format: [4-byte magic][1-byte version][12-byte nonce][ciphertext+16-byte tag]
func encryptStream(r io.Reader, key []byte) ([]byte, error) {
	plaintext, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	var buf bytes.Buffer
	buf.Write(encryptionMagic)   // 4 bytes
	buf.WriteByte(0x01)          // version
	buf.Write(nonce)             // 12 bytes
	buf.Write(ciphertext)        // ciphertext + 16-byte tag
	return buf.Bytes(), nil
}

// decryptBytes decrypts an AES-256-GCM encrypted blob produced by encryptStream.
func decryptBytes(data []byte, key []byte) ([]byte, error) {
	if len(data) < 4+1+12+16 {
		return nil, fmt.Errorf("encrypted file too short")
	}
	if !bytes.Equal(data[:4], encryptionMagic) {
		return nil, fmt.Errorf("not a Freezer encrypted file")
	}
	// data[4] = version (reserved, currently 0x01)
	nonce := data[5:17]
	ciphertext := data[17:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func connectFTP(host, user, pass string) (*ftp.ServerConn, error) {
	conn, err := ftp.Dial(host)
	if err != nil {
		return nil, err
	}
	if err := conn.Login(user, pass); err != nil {
		_ = conn.Quit()
		return nil, err
	}
	return conn, nil
}

func remotePathFromLocal(rel, ftpRoot string) string {
	clean := filepath.ToSlash(filepath.Clean(rel))
	clean = strings.TrimPrefix(clean, "/")
	clean = strings.ReplaceAll(clean, "\\", "/")
	base := strings.TrimSpace(ftpRoot)
	if base == "" {
		base = "/"
	}
	base = strings.ReplaceAll(base, "\\", "/")
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		base = "/"
	}
	if clean == "" {
		return base
	}
	if base == "/" {
		return "/" + strings.TrimPrefix(clean, "/")
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(clean, "/")
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// uploadFile uploads a local file to FTP, encrypting if encryption is configured.
// Returns (encrypted bool, error).
func uploadFile(client *ftp.ServerConn, localPath, remotePath string) (bool, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return false, err
	}
	defer f.Close()

	remoteDir := filepath.ToSlash(filepath.Dir(remotePath))
	if remoteDir != "." && remoteDir != "/" && remoteDir != "" {
		if err := ensureRemoteDir(client, remoteDir); err != nil {
			return false, err
		}
	}

	if appConfig.EncryptionEnabled && activeEncryptionKey != nil {
		ciphertext, err := encryptStream(f, activeEncryptionKey)
		if err != nil {
			return false, fmt.Errorf("encryption failed: %w", err)
		}
		if err := client.Stor(remotePath, bytes.NewReader(ciphertext)); err != nil {
			return false, err
		}
		return true, nil
	}

	// Refuse to upload plaintext if the user has encryption enabled but hasn't unlocked
	if appConfig.EncryptionEnabled && activeEncryptionKey == nil {
		return false, fmt.Errorf("encryption is enabled but not unlocked — use 'Unlock encryption...' from the tray menu before syncing")
	}

	return false, client.Stor(remotePath, f)
}

func ensureRemoteDir(client *ftp.ServerConn, dir string) error {
	if dir == "." || dir == "" || dir == "/" {
		return nil
	}
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	current := "/"
	for _, part := range parts {
		if part == "" {
			continue
		}
		current = strings.TrimRight(current, "/") + "/" + part
		if err := client.MakeDir(current); err != nil {
			// FTP servers return various messages when directory already exists
			e := strings.ToLower(err.Error())
			if !strings.Contains(e, "already exists") &&
				!strings.Contains(e, "file exists") &&
				!strings.Contains(e, "exists") &&
				!strings.Contains(e, "550") {
				return err
			}
		}
	}
	return nil
}

func createPlaceholder(path string, rec Record) error {
	frozenPath := path + ".frozen"
	f, err := os.Create(frozenPath)
	if err != nil {
		return err
	}
	defer f.Close()
	archivedDate := rec.ExpiresAt.Add(-retentionDuration()).Format("2006-01-02")
	_, err = fmt.Fprintf(f,
		"[Freezer Archive]\nThis file has been moved to cold storage.\nOriginal: %s\nArchived: %s\nRemote: %s\n\nDouble-click this file to restore it, or use the Freezer tray menu.\n",
		filepath.Base(path), archivedDate, rec.RemotePath,
	)
	return err
}

func archiveLocalFile(path string, rec Record) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return createPlaceholder(path, rec)
}

func restoreFile(client *ftp.ServerConn, localPath string, rec Record) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}

	resp, err := client.Retr(rec.RemotePath)
	if err != nil {
		return err
	}
	defer resp.Close()

	if rec.Encrypted && activeEncryptionKey != nil {
		ciphertext, err := io.ReadAll(resp)
		if err != nil {
			return err
		}
		plaintext, err := decryptBytes(ciphertext, activeEncryptionKey)
		if err != nil {
			return fmt.Errorf("decryption failed: %w", err)
		}
		if err := os.WriteFile(localPath, plaintext, 0644); err != nil {
			return err
		}
	} else if rec.Encrypted && activeEncryptionKey == nil {
		return fmt.Errorf("file %s is encrypted but encryption is locked; use 'Unlock encryption' from the tray menu", localPath)
	} else {
		out, err := os.Create(localPath)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err := io.Copy(out, resp); err != nil {
			return err
		}
	}

	os.Remove(localPath + ".frozen")
	return nil
}

func syncFolder(root string, client *ftp.ServerConn, state *State) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if filepath.Base(path) == metadataDirName {
				return filepath.SkipDir
			}
			// Skip excluded folders (matched by relative path from root)
			if rel, relErr := filepath.Rel(root, path); relErr == nil && rel != "." {
				for _, ex := range appConfig.ExcludedFolders {
					if rel == ex || strings.HasPrefix(rel, ex+string(filepath.Separator)) {
						return filepath.SkipDir
					}
				}
			}
			return nil
		}
		if filepath.Base(path) == metadataDirName {
			return nil
		}
		// Skip .frozen placeholder stubs — they are not real file content
		if strings.HasSuffix(path, ".frozen") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		remotePath := remotePathFromLocal(rel, appConfig.FTPRoot)

		record, ok := state.Get(path)
		now := time.Now()
		if !ok || record.Placeholder || record.LastModified.Before(info.ModTime()) || now.After(record.ExpiresAt) {
			contentHash, err := hashFile(path)
			if err != nil {
				return err
			}
			encrypted, err := uploadFile(client, path, remotePath)
			if err != nil {
				return err
			}
			if err := state.Put(path, Record{
				LocalPath:    path,
				RemotePath:   remotePath,
				UploadedAt:   now,
				ExpiresAt:    now.Add(retentionDuration()),
				Placeholder:  false,
				ContentHash:  contentHash,
				LastModified: info.ModTime(),
				FileSize:     info.Size(),
				Encrypted:    encrypted,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func pruneExpired(client *ftp.ServerConn, state *State) error {
	now := time.Now()
	return state.ForEach(func(localPath string, record Record) error {
		if record.Placeholder {
			return nil
		}
		if !record.IsExpired(now) {
			return nil
		}
		noVerification := !appConfig.VerifyUploadSize && !appConfig.VerifyUploadHash
		if noVerification && !appConfig.AllowUnsafeFreeze {
			log.Printf("skip prune for %s: no integrity checks enabled and allow-unsafe-freeze is off", localPath)
			return nil
		}
		remoteSize, err := client.FileSize(record.RemotePath)
		if err != nil {
			log.Printf("skip prune for %s: remote file missing - %v", localPath, err)
			return nil
		}
		if appConfig.VerifyUploadSize && record.FileSize > 0 && remoteSize != record.FileSize {
			log.Printf("skip prune for %s: size mismatch local=%d remote=%d", localPath, record.FileSize, remoteSize)
			return nil
		}
		if appConfig.VerifyUploadHash && appConfig.HashCommand != "" && record.ContentHash != "" {
			if err := verifyRemoteHash(appConfig.Host, appConfig.User, appConfig.Pass, record.RemotePath, appConfig.HashCommand, record.ContentHash); err != nil {
				log.Printf("skip prune for %s: hash verification failed - %v", localPath, err)
				return nil
			}
		}
		if err := archiveLocalFile(localPath, record); err != nil {
			return err
		}
		record.Placeholder = true
		record.ExpiresAt = now
		return state.Put(localPath, record)
	})
}

func restorePlaceholderIfNeeded(path string, client *ftp.ServerConn, state *State) error {
	record, ok := state.Get(path)
	if !ok {
		return nil
	}
	if !record.Placeholder {
		return nil
	}
	if err := restoreFile(client, path, record); err != nil {
		return err
	}
	record.Placeholder = false
	record.UploadedAt = time.Now()
	record.ExpiresAt = time.Now().Add(retentionDuration())
	return state.Put(path, record)
}

// FTPCapabilities describes which integrity commands the FTP server supports.
type FTPCapabilities struct {
	SIZE    bool // always true for modern servers
	HASH    bool // RFC 3659 HASH extension
	XMD5    bool // common proprietary extension
	XCRC    bool // common proprietary extension
	XSHA256 bool // common proprietary extension
}

// BestHashCommand returns the most reliable hash command available, or "".
func (c FTPCapabilities) BestHashCommand() string {
	switch {
	case c.HASH:
		return "HASH"
	case c.XSHA256:
		return "XSHA256"
	case c.XMD5:
		return "XMD5"
	case c.XCRC:
		return "XCRC"
	default:
		return ""
	}
}

// Summary returns a human-readable list of detected capabilities.
func (c FTPCapabilities) Summary() string {
	var parts []string
	if c.SIZE {
		parts = append(parts, "SIZE")
	}
	if c.HASH {
		parts = append(parts, "HASH")
	}
	if c.XSHA256 {
		parts = append(parts, "XSHA256")
	}
	if c.XMD5 {
		parts = append(parts, "XMD5")
	}
	if c.XCRC {
		parts = append(parts, "XCRC")
	}
	if len(parts) == 0 {
		return "No integrity commands detected"
	}
	return "Detected: " + strings.Join(parts, ", ")
}

// readFTPResponse reads a (possibly multi-line) FTP response from the connection.
func readFTPResponse(r *bufio.Reader) string {
	var sb strings.Builder
	for {
		line, err := r.ReadString('\n')
		sb.WriteString(line)
		if err != nil || (len(line) >= 4 && line[3] == ' ') {
			break
		}
	}
	return sb.String()
}

// probeFTPCapabilities connects to the FTP server and probes FEAT to discover
// which integrity commands are available.
func probeFTPCapabilities(host, user, pass string) (FTPCapabilities, error) {
	caps := FTPCapabilities{SIZE: true}

	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return caps, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))

	r := bufio.NewReader(conn)
	readFTPResponse(r) // greeting

	if user != "" {
		fmt.Fprintf(conn, "USER %s\r\n", user)
		readFTPResponse(r)
		fmt.Fprintf(conn, "PASS %s\r\n", pass)
		readFTPResponse(r)
	}

	fmt.Fprintf(conn, "FEAT\r\n")
	featResp := readFTPResponse(r)

	for _, line := range strings.Split(featResp, "\n") {
		feat := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(feat, "HASH"):
			caps.HASH = true
		case strings.HasPrefix(feat, "XMD5"):
			caps.XMD5 = true
		case strings.HasPrefix(feat, "XCRC"):
			caps.XCRC = true
		case strings.HasPrefix(feat, "XSHA256"):
			caps.XSHA256 = true
		}
	}

	return caps, nil
}

// verifyRemoteHash sends a hash command to the FTP server over a raw TCP connection
// and compares the result against the locally stored hash.
func verifyRemoteHash(host, user, pass, remotePath, command, expectedHash string) error {
	conn, err := net.DialTimeout("tcp", host, 15*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))

	r := bufio.NewReader(conn)
	readFTPResponse(r)
	fmt.Fprintf(conn, "USER %s\r\n", user)
	readFTPResponse(r)
	fmt.Fprintf(conn, "PASS %s\r\n", pass)
	readFTPResponse(r)
	fmt.Fprintf(conn, "%s %s\r\n", command, remotePath)
	resp := strings.TrimSpace(readFTPResponse(r))

	// Hash response formats vary: "251 hash" or "213 algo hash"
	parts := strings.Fields(resp)
	if len(parts) < 2 {
		return fmt.Errorf("unexpected hash response: %s", resp)
	}
	remoteHash := strings.ToLower(parts[len(parts)-1])
	if remoteHash != strings.ToLower(expectedHash) {
		return fmt.Errorf("hash mismatch: expected %s got %s", expectedHash, remoteHash)
	}
	return nil
}

// restoreSingleFile restores one .frozen placeholder back to its original content.
// Called when the user double-clicks a .frozen file via shell integration.
func restoreSingleFile(frozenPath string) error {
	originalPath := strings.TrimSuffix(frozenPath, ".frozen")

	state := newState()
	if err := state.Open(dbPath(appConfig.Root)); err != nil {
		return fmt.Errorf("cannot open state: %w", err)
	}
	defer state.Close()

	record, ok := state.Get(originalPath)
	if !ok || !record.Placeholder {
		return fmt.Errorf("no archive record found for %s", originalPath)
	}

	client, err := connectFTP(appConfig.Host, appConfig.User, appConfig.Pass)
	if err != nil {
		return err
	}
	defer client.Quit()

	if err := restoreFile(client, originalPath, record); err != nil {
		return err
	}

	record.Placeholder = false
	record.UploadedAt = time.Now()
	record.ExpiresAt = time.Now().Add(retentionDuration())
	return state.Put(originalPath, record)
}

// installShellIntegration registers .frozen files with the OS so double-clicking restores them.
func installShellIntegration() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	if runtime.GOOS == "windows" {
		return installShellIntegrationWindows(exePath)
	}
	return installShellIntegrationLinux(exePath)
}

func installShellIntegrationWindows(exePath string) error {
	openCmd := `"` + exePath + `" -restore "%1"`
	cmds := [][]string{
		{"reg", "add", `HKCU\Software\Classes\.frozen`, "/ve", "/d", "FreezerPlaceholder", "/f"},
		{"reg", "add", `HKCU\Software\Classes\FreezerPlaceholder`, "/ve", "/d", "Freezer Archive File", "/f"},
		{"reg", "add", `HKCU\Software\Classes\FreezerPlaceholder\DefaultIcon`, "/ve", "/d", exePath + ",0", "/f"},
		{"reg", "add", `HKCU\Software\Classes\FreezerPlaceholder\shell\open\command`, "/ve", "/d", openCmd, "/f"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("reg command failed: %v: %s", err, out)
		}
	}
	return nil
}

func installShellIntegrationLinux(exePath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	// Register MIME type
	mimeDir := filepath.Join(home, ".local", "share", "mime", "packages")
	if err := os.MkdirAll(mimeDir, 0755); err != nil {
		return err
	}
	mimeXML := `<?xml version="1.0" encoding="UTF-8"?>
<mime-info xmlns="http://www.freedesktop.org/standards/shared-mime-info">
  <mime-type type="application/x-freezer-placeholder">
    <comment>Freezer Archive Placeholder</comment>
    <glob pattern="*.frozen"/>
  </mime-type>
</mime-info>`
	if err := os.WriteFile(filepath.Join(mimeDir, "freezer.xml"), []byte(mimeXML), 0644); err != nil {
		return err
	}

	// Register .desktop handler
	appDir := filepath.Join(home, ".local", "share", "applications")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}
	desktop := fmt.Sprintf("[Desktop Entry]\nName=Freezer Restore\nExec=%s -restore %%f\nType=Application\nMimeType=application/x-freezer-placeholder;\nNoDisplay=true\n", exePath)
	if err := os.WriteFile(filepath.Join(appDir, "freezer-restore.desktop"), []byte(desktop), 0644); err != nil {
		return err
	}

	// Update system databases (errors are non-fatal)
	exec.Command("update-mime-database", filepath.Join(home, ".local", "share", "mime")).Run()
	exec.Command("update-desktop-database", appDir).Run()
	exec.Command("xdg-mime", "default", "freezer-restore.desktop", "application/x-freezer-placeholder").Run()
	return nil
}

// preventSleep starts an inhibitor process to prevent machine sleep
// Returns a cleanup function to stop the inhibitor
func preventSleep() func() {
	var cmd *exec.Cmd
	
	if runtime.GOOS == "windows" {
		// Windows: Use PowerShell to prevent sleep via Windows API
		psScript := `
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;

public class WindowsAPI {
    [DllImport("kernel32.dll", CharSet = CharSet.Auto, SetLastError = true)]
    public static extern void SetThreadExecutionState(uint esFlags);
}
"@

[WindowsAPI]::SetThreadExecutionState(0x80000003)
Start-Sleep -Seconds 999999
`
		cmd = exec.Command("powershell", "-NoProfile", "-Command", psScript)
	} else {
		// Linux/macOS: Use systemd-inhibit
		cmd = exec.Command("systemd-inhibit", "--why=Freezer sync in progress", "--mode=block", "sleep", "infinity")
	}
	
	if err := cmd.Start(); err != nil {
		// Command not available, silently fail
		return func() {}
	}
	
	return func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}
}

// backupIndexToFTP uploads the local index.db to FTP as a dated backup.
func backupIndexToFTP(root, host, user, pass, ftpRoot string) error {
	db := dbPath(root)
	f, err := os.Open(db)
	if err != nil {
		return err
	}
	defer f.Close()

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	datestamp := time.Now().Format("2006-01-02")
	remote := filepath.ToSlash(filepath.Join(ftpRoot, ".freezer-backups",
		fmt.Sprintf("index-%s-%s.db", hostname, datestamp)))

	client, err := connectFTP(host, user, pass)
	if err != nil {
		return err
	}
	defer client.Quit()

	if err := ensureRemoteDir(client, filepath.ToSlash(filepath.Dir(remote))); err != nil {
		return err
	}
	if err := client.Stor(remote, f); err != nil {
		return err
	}
	log.Printf("index backup uploaded to %s", remote)

	// Update backup timestamp in config
	appConfig.LastIndexBackup = time.Now()
	_ = saveConfig(appConfig)
	return nil
}

func runColdStorage(root, host, user, pass string) error {
	var cleanup func()
	if appConfig.PreventSleepDuringSync {
		cleanup = preventSleep()
		defer cleanup()
	}

	state := newState()
	if err := state.Open(dbPath(root)); err != nil {
		return err
	}
	defer state.Close()

	client, err := connectFTP(host, user, pass)
	if err != nil {
		return err
	}
	defer client.Quit()

	if err := syncFolder(root, client, state); err != nil {
		return err
	}
	if err := pruneExpired(client, state); err != nil {
		return err
	}

	// Weekly index backup to FTP
	if time.Since(appConfig.LastIndexBackup) >= 7*24*time.Hour {
		if err := backupIndexToFTP(root, host, user, pass, appConfig.FTPRoot); err != nil {
			log.Printf("index backup failed: %v", err)
		}
	}

	return nil
}

func restorePlaceholderFiles(root, host, user, pass string) error {
	state := newState()
	if err := state.Open(dbPath(root)); err != nil {
		return err
	}
	defer state.Close()

	client, err := connectFTP(host, user, pass)
	if err != nil {
		return err
	}
	defer client.Quit()

	return state.ForEach(func(localPath string, record Record) error {
		if !record.Placeholder {
			return nil
		}
		if err := restoreFile(client, localPath, record); err != nil {
			return err
		}
		record.Placeholder = false
		record.UploadedAt = time.Now()
		record.ExpiresAt = time.Now().Add(retentionDuration())
		return state.Put(localPath, record)
	})
}

func openFolder(path string) {
	if err := exec.Command("xdg-open", path).Start(); err != nil {
		log.Printf("cannot open folder %s: %v", path, err)
	}
}

func drawLine(img *image.RGBA, x0, y0, x1, y1 int, col color.RGBA) {
	dx := abs(x1 - x0)
	dy := abs(y1 - y0)
	sx, sy := 1, 1
	if x0 >= x1 {
		sx = -1
	}
	if y0 >= y1 {
		sy = -1
	}
	err := dx - dy
	for {
		if x0 >= 0 && x0 < img.Bounds().Dx() && y0 >= 0 && y0 < img.Bounds().Dy() {
			img.Set(x0, y0, col)
		}
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func makeTrayIconData() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	bg := color.RGBA{R: 25, G: 118, B: 210, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)

	white := color.RGBA{R: 255, G: 255, B: 255, A: 255}
	cx, cy := 32, 32
	lineLen := 18
	for _, d := range []struct{ x1, y1, x2, y2 int }{
		{cx, cy - lineLen, cx, cy + lineLen},
		{cx - lineLen, cy, cx + lineLen, cy},
		{cx - lineLen, cy - lineLen, cx + lineLen, cy + lineLen},
		{cx - lineLen, cy + lineLen, cx + lineLen, cy - lineLen},
		{cx - 12, cy - 17, cx + 12, cy + 17},
		{cx + 12, cy - 17, cx - 12, cy + 17},
	} {
		drawLine(img, d.x1, d.y1, d.x2, d.y2, white)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Printf("failed to encode tray icon: %v", err)
		return nil
	}
	return buf.Bytes()
}

func makeTrayIconICO() []byte {
	pngData := makeTrayIconData()
	if len(pngData) == 0 {
		return nil
	}

	// Wrap PNG bytes in an ICO container.
	// Windows Vista+ supports PNG images embedded directly in ICO files.
	dataSize := uint32(len(pngData))
	dataOffset := uint32(6 + 16) // ICONDIR (6 bytes) + one ICONDIRENTRY (16 bytes)

	var buf bytes.Buffer

	// ICONDIR header (6 bytes)
	buf.Write([]byte{0, 0})  // idReserved
	buf.Write([]byte{1, 0})  // idType = 1 (ICO)
	buf.Write([]byte{1, 0})  // idCount = 1

	// ICONDIRENTRY (16 bytes)
	buf.WriteByte(64)         // bWidth
	buf.WriteByte(64)         // bHeight
	buf.WriteByte(0)          // bColorCount (0 = PNG)
	buf.WriteByte(0)          // bReserved
	buf.Write([]byte{1, 0})  // wPlanes
	buf.Write([]byte{32, 0}) // wBitCount
	buf.Write([]byte{       // dwBytesInRes
		byte(dataSize), byte(dataSize >> 8),
		byte(dataSize >> 16), byte(dataSize >> 24),
	})
	buf.Write([]byte{       // dwImageOffset
		byte(dataOffset), byte(dataOffset >> 8),
		byte(dataOffset >> 16), byte(dataOffset >> 24),
	})

	buf.Write(pngData)
	return buf.Bytes()
}

func setTrayIcon() {
	systray.SetTooltip("Freezer sync")
	systray.SetTitle("")

	// getlantern/systray has a known bug on Windows where it misreports
	// successful icon API calls as errors. Skip icon on Windows to avoid noise.
	if runtime.GOOS == "windows" {
		return
	}

	iconData := makeTrayIconData()
	if len(iconData) == 0 {
		return
	}
	systray.SetIcon(iconData)
	systray.SetTemplateIcon(iconData, iconData)
}

type compactTheme struct {
	fyne.Theme
}

func (t compactTheme) TextSize() float32           { return 12 }
func (t compactTheme) TextSubHeadingSize() float32 { return 14 }
func (t compactTheme) TextHeadingSize() float32    { return 18 }
func (t compactTheme) Size(name fyne.ThemeSizeName) float32 {
	if name == theme.SizeNameText {
		return 12
	}
	return t.Theme.Size(name)
}

func launchSettingsProcess() error {
	cmd := exec.Command(os.Args[0], "--settings")
	cmd.Env = os.Environ()
	return cmd.Start()
}

func openSettingsWindow() {
	if err := launchSettingsProcess(); err != nil {
		log.Printf("failed to launch settings window: %v", err)
	}
}

func refreshConfig() {
	cfg, err := readPersistedConfig()
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("failed to reload config: %v", err)
		}
		return
	}
	appConfig = cfg
}

func getCommonFolders() map[string]string {
	folders := make(map[string]string)
	
	if home, err := os.UserHomeDir(); err == nil {
		folders["Home"] = home
	}
	if desktop, err := os.UserHomeDir(); err == nil {
		folders["Desktop"] = filepath.Join(desktop, "Desktop")
	}
	if docs, err := os.UserHomeDir(); err == nil {
		folders["Documents"] = filepath.Join(docs, "Documents")
	}
	if downloads, err := os.UserHomeDir(); err == nil {
		folders["Downloads"] = filepath.Join(downloads, "Downloads")
	}
	
	return folders
}

func chooseLocalFolder(parent fyne.Window, currentPath string, onSelect func(string)) {
	a := fyne.CurrentApp()
	if a == nil {
		return
	}

	picker := a.NewWindow("Select Folder")
	picker.Resize(fyne.NewSize(900, 600))
	picker.SetFixedSize(false)
	picker.SetCloseIntercept(func() {
		picker.Close()
	})

	var currentFolder string
	if currentPath == "" {
		currentPath = "."
	}
	absFolder, err := filepath.Abs(currentPath)
	if err == nil {
		currentFolder = absFolder
	} else {
		currentFolder = currentPath
	}

	// Navigation history for back/forward
	history := []string{currentFolder}
	historyIndex := 0

	// UI elements
	folderDisplay := container.NewVBox()
	scrollContainer := container.NewScroll(folderDisplay)
	pathEntry := widget.NewEntry()
	pathEntry.SetPlaceHolder("Enter folder path...")

	var refreshFolder func(string)
	var updatePathDisplay func()

	// Create breadcrumb navigation
	breadcrumbContainer := container.NewHBox()
	updateBreadcrumb := func() {
		breadcrumbContainer.Objects = []fyne.CanvasObject{}
		parts := strings.Split(strings.TrimPrefix(currentFolder, "/"), "/")
		if currentFolder == "/" {
			parts = []string{""}
		}
		
		path := ""
		for i, part := range parts {
			if part == "" && i == 0 {
				p := "/"
				btn := widget.NewButton(p, func(targetPath string) func() {
					return func() { refreshFolder(targetPath) }
				}("/"))
				btn.Importance = widget.LowImportance
				breadcrumbContainer.Add(btn)
			} else if part != "" {
				path = filepath.Join(path, part)
				btnLabel := part
				if i < len(parts)-1 {
					btnLabel = part + " >"
				}
				btn := widget.NewButton(btnLabel, func(targetPath string) func() {
					return func() { refreshFolder(targetPath) }
				}(path))
				btn.Importance = widget.LowImportance
				breadcrumbContainer.Add(btn)
			}
		}
	}

	updatePathDisplay = func() {
		pathEntry.SetText(currentFolder)
		updateBreadcrumb()
		breadcrumbContainer.Refresh()
	}

	refreshFolder = func(folder string) {
		currentFolder = folder
		if currentFolder == "" {
			currentFolder = "."
		}
		absFolder, err := filepath.Abs(currentFolder)
		if err == nil {
			currentFolder = absFolder
		}

		// Update history
		if historyIndex < len(history)-1 {
			history = history[:historyIndex+1]
		}
		if len(history) == 0 || history[len(history)-1] != currentFolder {
			history = append(history, currentFolder)
			historyIndex = len(history) - 1
		}

		updatePathDisplay()

		// Refresh folder contents
		folderDisplay.Objects = []fyne.CanvasObject{}
		entries, err := os.ReadDir(currentFolder)
		if err != nil {
			folderDisplay.Add(widget.NewLabel("Unable to read folder"))
		} else {
			var dirs []string
			for _, entry := range entries {
				if entry.IsDir() {
					dirs = append(dirs, entry.Name())
				}
			}
			sort.Strings(dirs)

			if len(dirs) == 0 {
				folderDisplay.Add(widget.NewLabel("No subfolders"))
			} else {
				for _, name := range dirs {
					if name == metadataDirName {
						continue
					}
					if strings.HasPrefix(name, ".") {
						continue
					}
					fullPath := filepath.Join(currentFolder, name)
					folderName := name
					btn := widget.NewButton("📁 " + folderName, func(path string) func() {
						return func() { refreshFolder(path) }
					}(fullPath))
					folderDisplay.Add(btn)
				}
			}
		}
		folderDisplay.Refresh()
		scrollContainer.Refresh()
	}

	// Sidebar with quick access locations
	sidebar := container.NewVBox(
		widget.NewLabelWithStyle("Quick Access", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	)
	commonFolders := getCommonFolders()
	for name, path := range map[string]string{"Home": commonFolders["Home"], "Desktop": commonFolders["Desktop"], "Documents": commonFolders["Documents"], "Downloads": commonFolders["Downloads"]} {
		if path != "" {
			btn := widget.NewButton(name, func(p string) func() {
				return func() { refreshFolder(p) }
			}(path))
			btn.Importance = widget.LowImportance
			sidebar.Add(btn)
		}
	}

	// Navigation toolbar
	backBtn := widget.NewButton("← Back", func() {
		if historyIndex > 0 {
			historyIndex--
			currentFolder = history[historyIndex]
			updatePathDisplay()
			folderDisplay.Objects = []fyne.CanvasObject{}
			entries, _ := os.ReadDir(currentFolder)
			var dirs []string
			for _, entry := range entries {
				if entry.IsDir() {
					dirs = append(dirs, entry.Name())
				}
			}
			sort.Strings(dirs)
			for _, name := range dirs {
				if name == metadataDirName {
					continue
				}
				fullPath := filepath.Join(currentFolder, name)
				btn := widget.NewButton("📁 " + name, func(path string) func() {
					return func() { refreshFolder(path) }
				}(fullPath))
				folderDisplay.Add(btn)
			}
			folderDisplay.Refresh()
			scrollContainer.Refresh()
		}
	})
	forwardBtn := widget.NewButton("Forward →", func() {
		if historyIndex < len(history)-1 {
			historyIndex++
			currentFolder = history[historyIndex]
			updatePathDisplay()
			folderDisplay.Objects = []fyne.CanvasObject{}
			entries, _ := os.ReadDir(currentFolder)
			var dirs []string
			for _, entry := range entries {
				if entry.IsDir() {
					dirs = append(dirs, entry.Name())
				}
			}
			sort.Strings(dirs)
			for _, name := range dirs {
				if name == metadataDirName {
					continue
				}
				fullPath := filepath.Join(currentFolder, name)
				btn := widget.NewButton("📁 " + name, func(path string) func() {
					return func() { refreshFolder(path) }
				}(fullPath))
				folderDisplay.Add(btn)
			}
			folderDisplay.Refresh()
			scrollContainer.Refresh()
		}
	})
	upBtn := widget.NewButton("⬆ Up", func() {
		parent := filepath.Dir(currentFolder)
		if parent != currentFolder {
			refreshFolder(parent)
		}
	})
	for _, b := range []*widget.Button{backBtn, forwardBtn, upBtn} {
		b.Importance = widget.LowImportance
	}

	toolbar := container.NewHBox(backBtn, forwardBtn, upBtn, layout.NewSpacer())

	// Path entry handler
	pathEntry.OnSubmitted = func(text string) {
		if text != "" {
			refreshFolder(text)
		}
	}

	// Bottom buttons
	selectButton := widget.NewButton("Select Folder", func() {
		picked := strings.TrimSpace(pathEntry.Text)
		if picked == "" {
			picked = currentFolder
		}
		onSelect(picked)
		picker.Close()
	})
	selectButton.Importance = widget.HighImportance
	cancelButton := widget.NewButton("Cancel", func() {
		picker.Close()
	})
	bottomBar := container.NewHBox(
		layout.NewSpacer(),
		selectButton,
		cancelButton,
	)

	// Initial refresh
	refreshFolder(currentFolder)

	// Main layout: sidebar on left, main content on right
	mainContent := container.NewBorder(
		container.NewVBox(toolbar, pathEntry),
		bottomBar,
		sidebar,
		nil,
		scrollContainer,
	)

	picker.SetContent(mainContent)
	picker.Show()
}

func runSettingsWindow() {
	a := app.NewWithID("com.freezer.settings")
	a.Settings().SetTheme(compactTheme{Theme: theme.DefaultTheme()})
	w := a.NewWindow("Freezer Settings")
	w.Resize(fyne.NewSize(820, 470))
	w.SetFixedSize(false)
	w.SetCloseIntercept(func() {
		w.Close()
		a.Quit()
	})

	rootEntry := widget.NewEntry()
	rootEntry.SetText(appConfig.Root)
	rootEntry.SetPlaceHolder("Choose a local sync folder")
	hostEntry := widget.NewEntry()
	hostEntry.SetText(appConfig.Host)
	hostEntry.SetPlaceHolder("ftp.example.com:21")
	userEntry := widget.NewEntry()
	userEntry.SetText(appConfig.User)
	userEntry.SetPlaceHolder("username")
	passwordEntry := widget.NewPasswordEntry()
	passwordEntry.SetText(appConfig.Pass)
	passwordEntry.SetPlaceHolder("Password")
	ftpRootEntry := widget.NewEntry()
	ftpRootEntry.SetText(appConfig.FTPRoot)
	ftpRootEntry.SetPlaceHolder("/")
	batteryCheck := widget.NewCheck("Sync on battery", func(checked bool) {})
	batteryCheck.Checked = appConfig.SyncOnBattery
	batteryCheck.SetChecked(appConfig.SyncOnBattery)
	preventSleepCheck := widget.NewCheck("Prevent sleep during sync", func(checked bool) {})
	preventSleepCheck.Checked = appConfig.PreventSleepDuringSync
	preventSleepCheck.SetChecked(appConfig.PreventSleepDuringSync)

	browseButton := widget.NewButton("Browse", func() {
		chooseLocalFolder(w, rootEntry.Text, func(path string) {
			rootEntry.SetText(path)
		})
	})
	browseButton.Importance = widget.LowImportance

	powerPanel := container.NewVBox(
		widget.NewLabelWithStyle("Power", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Machine-level power behavior."),
		container.NewPadded(batteryCheck),
		container.NewPadded(preventSleepCheck),
	)

	weekdayLabels := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	weekdayChecks := make([]*widget.Check, len(weekdayLabels))
	for i, label := range weekdayLabels {
		weekdayChecks[i] = widget.NewCheck(label, func(checked bool) {})
		weekdayChecks[i].Checked = appConfig.Weekdays[i]
		weekdayChecks[i].SetChecked(appConfig.Weekdays[i])
	}
	throughputStatus := widget.NewLabel("No throughput test run yet.")
	throughputProgress := widget.NewProgressBarInfinite()
	throughputProgress.Hide() // Hidden by default
	
	// Container to hold status and progress spinner
	throughputContainer := container.NewVBox(
		throughputStatus,
		throughputProgress,
	)
	
	throughputButton := widget.NewButton("Test throughput", nil)
	
	throughputButton.OnTapped = func() {
		if appConfig.Host == "" || strings.Contains(appConfig.Host, "example.com") {
			dialog.NewError(fmt.Errorf("enter a real FTP server before testing throughput"), w).Show()
			return
		}
		if appConfig.User == "" {
			dialog.NewError(fmt.Errorf("FTP username is required"), w).Show()
			return
		}
		
		// Show progress spinner and disable button
		throughputButton.Disable()
		throughputStatus.SetText("Testing FTP throughput...")
		throughputProgress.Show()
		
		// Run test in goroutine so UI can render the progress spinner
		go func() {
			uploadMbps, downloadMbps, err := measureFTPThroughput(appConfig.Host, appConfig.User, appConfig.Pass, appConfig.FTPRoot)
			
			// Hide progress spinner
			throughputProgress.Hide()
			
			if err != nil {
				throughputStatus.SetText(fmt.Sprintf("Throughput test failed: %v", err))
			} else {
				throughputStatus.SetText(fmt.Sprintf("Upload: %.2f Mb/s  |  Download: %.2f Mb/s", uploadMbps, downloadMbps))
			}
			
			// Re-enable button
			throughputButton.Enable()
		}()
	}
	schedulePanel := container.NewVBox(
		widget.NewLabelWithStyle("Sync Schedule", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Syncs start nightly at 11:00 PM."),
		widget.NewLabel("Initial sync must be kicked off manually from the tray menu."),
		container.NewGridWithColumns(7,
			weekdayChecks[0],
			weekdayChecks[1],
			weekdayChecks[2],
			weekdayChecks[3],
			weekdayChecks[4],
			weekdayChecks[5],
			weekdayChecks[6],
		),
		container.NewHBox(throughputButton),
		container.NewPadded(throughputContainer),
	)

	ftpPanel := container.NewVBox(
		widget.NewLabelWithStyle("FTP & Folder Settings", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewBorder(nil, nil, nil, browseButton, rootEntry),
		widget.NewLabel("FTP server"),
		hostEntry,
		widget.NewLabel("Username"),
		userEntry,
		widget.NewLabel("Password"),
		passwordEntry,
		widget.NewLabel("FTP root folder"),
		ftpRootEntry,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Server Capabilities", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Probe the server to discover which integrity checks are available."),
	)

	capStatusLabel := widget.NewLabel("")
	verifySizeCheck := widget.NewCheck("Verify file size before archiving — SIZE (lowest level, confirms existence and completeness only)", func(bool) {})
	verifySizeCheck.SetChecked(appConfig.VerifyUploadSize)
	verifyHashCheck := widget.NewCheck("Verify file hash before archiving", func(bool) {})
	verifyHashCheck.SetChecked(appConfig.VerifyUploadHash)
	verifyHashCheck.Disable() // enabled only if a hash command is detected
	allowUnsafeCheck := widget.NewCheck("Allow freezing even when no integrity verification is available (at your own risk)", func(bool) {})
	allowUnsafeCheck.SetChecked(appConfig.AllowUnsafeFreeze)

	probeBtn := widget.NewButton("Probe server capabilities", nil)
	probeBtn.OnTapped = func() {
		host := strings.TrimSpace(hostEntry.Text)
		user := strings.TrimSpace(userEntry.Text)
		pass := strings.TrimSpace(passwordEntry.Text)
		if host == "" || strings.Contains(host, "example.com") {
			capStatusLabel.SetText("Enter a real FTP server first.")
			return
		}
		probeBtn.Disable()
		capStatusLabel.SetText("Probing...")
		go func() {
			caps, err := probeFTPCapabilities(host, user, pass)
			if err != nil {
				capStatusLabel.SetText("Probe failed: " + err.Error())
				probeBtn.Enable()
				return
			}
			capStatusLabel.SetText(caps.Summary())
			verifySizeCheck.SetChecked(caps.SIZE)
			if caps.BestHashCommand() != "" {
				verifyHashCheck.SetChecked(true)
				verifyHashCheck.Enable()
				verifyHashCheck.Text = "Verify file hash before archiving (" + caps.BestHashCommand() + ")"
				verifyHashCheck.Refresh()
			} else {
				verifyHashCheck.SetChecked(false)
				verifyHashCheck.Disable()
				verifyHashCheck.Text = "Verify file hash before archiving (not supported by server)"
				verifyHashCheck.Refresh()
			}
			probeBtn.Enable()
		}()
	}

	capPanel := container.NewVBox(
		container.NewHBox(probeBtn),
		container.NewPadded(capStatusLabel),
		container.NewPadded(verifySizeCheck),
		container.NewPadded(verifyHashCheck),
		container.NewPadded(allowUnsafeCheck),
	)

	ftpPanelFull := container.NewVBox(
		ftpPanel,
		capPanel,
	)

	// Exclusions panel
	excludedSet := make(map[string]bool)
	for _, ex := range appConfig.ExcludedFolders {
		excludedSet[ex] = true
	}
	var excludeCheckboxes []*widget.Check
	excludeList := container.NewVBox()
	excludeScroll := container.NewScroll(excludeList)
	excludeScroll.SetMinSize(fyne.NewSize(0, 220))

	var refreshExcludeList func(root string)
	refreshExcludeList = func(root string) {
		excludeList.Objects = nil
		excludeCheckboxes = nil
		if root == "" {
			excludeList.Add(widget.NewLabel("Select a root folder in FTP & Folder first."))
			excludeList.Refresh()
			return
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			excludeList.Add(widget.NewLabel("Cannot read root folder."))
			excludeList.Refresh()
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == metadataDirName {
				continue
			}
			name := entry.Name()
			chk := widget.NewCheck(name, func(bool) {})
			chk.SetChecked(excludedSet[name])
			excludeCheckboxes = append(excludeCheckboxes, chk)
			excludeList.Add(chk)
		}
		if len(excludeCheckboxes) == 0 {
			excludeList.Add(widget.NewLabel("No subfolders found in root."))
		}
		excludeList.Refresh()
		excludeScroll.Refresh()
	}
	refreshExcludeList(appConfig.Root)

	// Re-populate exclusions list when root folder changes
	rootEntry.OnChanged = func(text string) {
		refreshExcludeList(strings.TrimSpace(text))
	}

	exclusionsPanel := container.NewVBox(
		widget.NewLabelWithStyle("Excluded Folders", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Checked folders will be skipped during sync."),
		excludeScroll,
	)

	shellIntegrationStatus := widget.NewLabel("")
	installShellBtn := widget.NewButton("Install shell integration", func() {
		if err := installShellIntegration(); err != nil {
			shellIntegrationStatus.SetText("Failed: " + err.Error())
		} else {
			shellIntegrationStatus.SetText("Installed! Double-click any .frozen file to restore it.")
		}
	})
	systemPanel := container.NewVBox(
		widget.NewLabelWithStyle("System Integration", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Register .frozen files with the OS so double-clicking restores them."),
		container.NewHBox(installShellBtn),
		container.NewPadded(shellIntegrationStatus),
	)

	// Storage Settings panel - retention slider
	retentionDays := appConfig.RetentionDays
	if retentionDays <= 0 {
		retentionDays = defaultRetentionDays
	}
	retentionLabel := widget.NewLabel(fmt.Sprintf("Keep files warm for %d days before freezing", retentionDays))
	retentionSlider := widget.NewSlider(7, 365)
	retentionSlider.Step = 1
	retentionSlider.Value = float64(retentionDays)
	retentionSlider.OnChanged = func(v float64) {
		retentionDays = int(v)
		retentionLabel.SetText(fmt.Sprintf("Keep files warm for %d days before freezing", retentionDays))
	}

	// Only label the two endpoints — these align exactly with the slider ends
	tickRow := container.NewHBox(
		widget.NewLabelWithStyle("7 days", fyne.TextAlignLeading, fyne.TextStyle{}),
		layout.NewSpacer(),
		widget.NewLabelWithStyle("365 days", fyne.TextAlignTrailing, fyne.TextStyle{}),
	)

	storagePanel := container.NewVBox(
		widget.NewLabelWithStyle("Storage Settings", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Configure how long files stay on local disk before being archived to cold storage."),
		container.NewPadded(retentionLabel),
		retentionSlider,
		tickRow,
	)

	// Cryptography panel
	encryptCheck := widget.NewCheck("Encrypt files before uploading to FTP", func(bool) {})
	encryptCheck.SetChecked(appConfig.EncryptionEnabled)

	alreadyConfigured := appConfig.EncryptionSalt != ""

	passphraseEntry := widget.NewPasswordEntry()
	confirmEntry := widget.NewPasswordEntry()

	cryptoStatus := widget.NewLabel("")
	var passphraseLabel, confirmLabel *widget.Label

	if alreadyConfigured {
		passphraseEntry.SetPlaceHolder("Enter passphrase to unlock or change")
		confirmEntry.Hide()
		cryptoStatus.SetText("Encryption configured. Passphrase is not stored on disk.")
		passphraseLabel = widget.NewLabel("Enter passphrase to unlock for this session:")
		confirmLabel = widget.NewLabel("")
	} else {
		passphraseEntry.SetPlaceHolder("New passphrase")
		confirmEntry.SetPlaceHolder("Confirm passphrase")
		cryptoStatus.SetText("Passphrase is never stored on disk. You will be prompted on startup.")
		passphraseLabel = widget.NewLabel("Passphrase (Argon2id-derived AES-256 key, never stored on disk):")
		confirmLabel = widget.NewLabel("Confirm passphrase:")
	}

	unlockBtn := widget.NewButton("Unlock for this session", func() {
		p := strings.TrimSpace(passphraseEntry.Text)
		if p == "" {
			cryptoStatus.SetText("Passphrase cannot be empty.")
			return
		}
		key, err := deriveKey(p, appConfig.EncryptionSalt)
		if err != nil {
			cryptoStatus.SetText("Key derivation failed: " + err.Error())
			return
		}
		activeEncryptionKey = key
		cryptoStatus.SetText("Encryption unlocked for this session.")
		passphraseEntry.SetText("")
	})
	if !alreadyConfigured {
		unlockBtn.Hide()
	}

	genSaltBtn := widget.NewButton("Re-generate salt (invalidates existing archive)", func() {
		salt, err := newEncryptionSalt()
		if err != nil {
			cryptoStatus.SetText("Failed to generate salt: " + err.Error())
			return
		}
		appConfig.EncryptionSalt = salt
		activeEncryptionKey = nil
		cryptoStatus.SetText("New salt generated. Save and re-enter passphrase. All existing encrypted archives are no longer recoverable.")
	})
	genSaltBtn.Importance = widget.DangerImportance

	cryptoPanel := container.NewVBox(
		widget.NewLabelWithStyle("Cryptography", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("When enabled, file content is encrypted with AES-256-GCM before upload."),
		widget.NewLabel("The FTP server holds only ciphertext. Passphrase is never written to disk."),
		container.NewPadded(encryptCheck),
		passphraseLabel,
		passphraseEntry,
		confirmLabel,
		confirmEntry,
		container.NewHBox(unlockBtn),
		container.NewPadded(cryptoStatus),
		widget.NewSeparator(),
		widget.NewLabel("WARNING: if you lose your passphrase, encrypted archives cannot be recovered."),
		container.NewHBox(genSaltBtn),
	)

	sections := map[string]fyne.CanvasObject{
		"Power":         powerPanel,
		"Sync Schedule": schedulePanel,
		"FTP & Folder":  ftpPanelFull,
		"Exclusions":    exclusionsPanel,
		"Storage":       storagePanel,
		"Cryptography":  cryptoPanel,
		"System":        systemPanel,
	}
	order := []string{"Power", "Sync Schedule", "FTP & Folder", "Exclusions", "Storage", "Cryptography", "System"}
	selectedSection := "Power"
	rightPanel := container.NewVBox(sections[selectedSection])

	sidebar := container.NewVBox(
		widget.NewLabelWithStyle("Settings", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	)
	sidebarButtons := make(map[string]*widget.Button)
	for _, name := range order {
		btn := widget.NewButton(name, nil)
		if name == selectedSection {
			btn.Importance = widget.HighImportance
		}
		btn.OnTapped = func() {
			for _, s := range order {
				if b, ok := sidebarButtons[s]; ok {
					b.Importance = widget.LowImportance
					b.Refresh()
				}
			}
			btn.Importance = widget.HighImportance
			btn.Refresh()
			selectedSection = name
			rightPanel.Objects = []fyne.CanvasObject{sections[name]}
			rightPanel.Refresh()
		}
		sidebarButtons[name] = btn
		sidebar.Add(btn)
	}
	saveButton := widget.NewButton("Save", func() {
		cfg := appConfig
		cfg.Root = strings.TrimSpace(rootEntry.Text)
		cfg.Host = strings.TrimSpace(hostEntry.Text)
		cfg.User = strings.TrimSpace(userEntry.Text)
		cfg.Pass = strings.TrimSpace(passwordEntry.Text)
		cfg.FTPRoot = strings.TrimSpace(ftpRootEntry.Text)
		cfg.SyncOnBattery = batteryCheck.Checked
		cfg.PreventSleepDuringSync = preventSleepCheck.Checked
		cfg.RetentionDays = retentionDays
		cfg.VerifyUploadSize = verifySizeCheck.Checked
		cfg.VerifyUploadHash = verifyHashCheck.Checked
		cfg.AllowUnsafeFreeze = allowUnsafeCheck.Checked
		if verifyHashCheck.Checked {
			// Extract command name from checkbox label e.g. "...(XMD5)"
			label := verifyHashCheck.Text
			if i := strings.LastIndex(label, "("); i >= 0 {
				cfg.HashCommand = strings.TrimSuffix(label[i+1:], ")")
			}
		}
		// Cryptography — passphrase is NEVER saved to disk
		cfg.EncryptionEnabled = encryptCheck.Checked
		if encryptCheck.Checked && !alreadyConfigured {
			// First-time setup: validate passphrase, generate salt, derive key into memory
			p := strings.TrimSpace(passphraseEntry.Text)
			c := strings.TrimSpace(confirmEntry.Text)
			if p == "" {
				dialog.NewError(fmt.Errorf("passphrase cannot be empty when encryption is enabled"), w).Show()
				return
			}
			if p != c {
				dialog.NewError(fmt.Errorf("passphrases do not match"), w).Show()
				return
			}
			salt, err := newEncryptionSalt()
			if err != nil {
				dialog.NewError(fmt.Errorf("failed to generate encryption salt: %v", err), w).Show()
				return
			}
			cfg.EncryptionSalt = salt
			key, err := deriveKey(p, salt)
			if err != nil {
				dialog.NewError(fmt.Errorf("key derivation failed: %v", err), w).Show()
				return
			}
			activeEncryptionKey = key
		} else {
			// Preserve existing salt
			cfg.EncryptionSalt = appConfig.EncryptionSalt
		}
		var excluded []string
		for _, chk := range excludeCheckboxes {
			if chk.Checked {
				excluded = append(excluded, chk.Text)
			}
		}
		cfg.ExcludedFolders = excluded
		cfg.SyncHour = 23
		cfg.SyncMinute = 0
		for i, chk := range weekdayChecks {
			cfg.Weekdays[i] = chk.Checked
		}
		if cfg.FTPRoot == "" {
			cfg.FTPRoot = "/"
		}
		if cfg.Root == "" || cfg.Host == "" || strings.Contains(cfg.Host, "example.com") {
			dialog.NewError(fmt.Errorf("local folder and a real FTP server are required"), w).Show()
			return
		}
		if cfg.User == "" {
			dialog.NewError(fmt.Errorf("FTP username is required"), w).Show()
			return
		}
		if err := saveConfig(cfg); err != nil {
			dialog.NewError(err, w).Show()
			return
		}
		if err := os.MkdirAll(cfg.Root, 0755); err != nil {
			log.Printf("cannot create root folder %s: %v", cfg.Root, err)
		}
		appConfig = cfg
		w.Close()
		a.Quit()
	})
	cancelButton := widget.NewButton("Cancel", func() {
		w.Close()
		a.Quit()
	})
	bottomRow := container.NewHBox(
		layout.NewSpacer(),
		saveButton,
		cancelButton,
	)
	content := container.NewBorder(
		nil,
		container.NewPadded(bottomRow),
		sidebar,
		nil,
		container.NewPadded(rightPanel),
	)

	w.SetContent(content)
	w.Show()
	a.Run()
}

func onBatteryPower() bool {
	if _, err := exec.LookPath("upower"); err == nil {
		out, err := exec.Command("upower", "-e").Output()
		if err == nil && len(out) > 0 {
			if r, err := exec.Command("upower", "-i", "/org/freedesktop/UPower/devices/battery_BAT0").CombinedOutput(); err == nil {
				return strings.Contains(string(r), "state:         discharging") || strings.Contains(string(r), "state:         charging")
			}
		}
	}
	return false
}

func shouldSyncNow() bool {
	refreshConfig()
	if appConfig.SyncOnBattery {
		return true
	}
	return !onBatteryPower()
}

func isScheduledSyncWindow(now time.Time) bool {
	if appConfig.SyncHour < 0 || appConfig.SyncHour > 23 {
		appConfig.SyncHour = 23
	}
	if appConfig.SyncMinute < 0 || appConfig.SyncMinute > 59 {
		appConfig.SyncMinute = 0
	}
	if now.Hour() != appConfig.SyncHour || now.Minute() != appConfig.SyncMinute {
		return false
	}
	weekdayIndex := int(now.Weekday()) - 1
	if weekdayIndex < 0 {
		weekdayIndex = 6
	}
	return appConfig.Weekdays[weekdayIndex]
}

func measureFTPThroughput(host, user, pass, ftpRoot string) (uploadMbps float64, downloadMbps float64, err error) {
	// Use separate connections for upload and download to avoid FTP state issues
	uploadClient, err := connectFTP(host, user, pass)
	if err != nil {
		return 0, 0, err
	}
	defer uploadClient.Quit()

	testName := ".freezer-test.bin"
	remotePath := remotePathFromLocal(testName, ftpRoot)
	
	// Larger payload for accurate throughput testing (10MB)
	// Small transfers are dominated by protocol overhead; 10MB gives stable measurements
	payload := bytes.Repeat([]byte("X"), 10485760) // 10MB
	payloadSize := len(payload)

	// Upload test
	uploadStart := time.Now()
	if err := uploadClient.Stor(remotePath, bytes.NewReader(payload)); err != nil {
		return 0, 0, err
	}
	uploadElapsed := time.Since(uploadStart).Seconds()
	if uploadElapsed <= 0 {
		uploadElapsed = 0.001
	}
	uploadMbps = (float64(payloadSize) * 8 / 1000000) / uploadElapsed

	// Give server time to sync the file
	time.Sleep(100 * time.Millisecond)

	// Create a fresh connection for download
	downloadClient, err := connectFTP(host, user, pass)
	if err != nil {
		return uploadMbps, 0, err
	}
	defer downloadClient.Quit()

	// Download test
	downloadStart := time.Now()
	resp, err := downloadClient.Retr(remotePath)
	if err != nil {
		return uploadMbps, 0, err
	}
	defer resp.Close()

	readBytes := 0
	buf := make([]byte, 32768)
	for {
		n, err := resp.Read(buf)
		readBytes += n
		if err == io.EOF {
			break
		}
		if err != nil {
			return uploadMbps, 0, err
		}
	}
	downloadElapsed := time.Since(downloadStart).Seconds()
	if downloadElapsed <= 0 {
		downloadElapsed = 0.001
	}
	downloadMbps = (float64(readBytes) * 8 / 1000000) / downloadElapsed

	// Clean up with the download client before closing
	downloadClient.Delete(remotePath)
	
	return uploadMbps, downloadMbps, nil
}

func showAboutWindow() {
	a := app.NewWithID("com.freezer.about")
	a.Settings().SetTheme(compactTheme{Theme: theme.DefaultTheme()})
	w := a.NewWindow("About Freezer")
	w.Resize(fyne.NewSize(620, 560))
	w.SetFixedSize(false)
	w.SetCloseIntercept(func() { w.Close(); a.Quit() })

	disclaimer := `DISCLAIMER — NO WARRANTY — LIMITATION OF LIABILITY

Freezer is provided "as is", without warranty of any kind, express or
implied, including but not limited to the warranties of merchantability,
fitness for a particular purpose, and non-infringement.

In no event shall the authors or copyright holders be liable for any
claim, damages, or other liability — whether in an action of contract,
tort, or otherwise — arising from, out of, or in connection with the
software or the use or other dealings in the software.

THIS INCLUDES BUT IS NOT LIMITED TO: loss of data, loss of encrypted
archives due to forgotten passphrases, FTP server outages, file
corruption, accidental deletion, interrupted sync operations, or any
financial loss resulting from inability to access archived files.

YOU ARE SOLELY RESPONSIBLE FOR:
  - Maintaining a secure backup of your encryption passphrase
  - Verifying that files have been successfully archived before deletion
  - Ensuring your FTP server is reliable and has sufficient storage
  - Any data that cannot be recovered due to key loss or server failure

Use this software at your own risk.`

	privacy := `PRIVACY POLICY

Freezer collects no data about you, your files, or your usage.

  - No telemetry
  - No analytics
  - No crash reporting
  - No usage statistics
  - No network connections except to your own configured FTP server
  - No third-party services of any kind

All data processed by Freezer stays on your local machine and your own
FTP server. The developers have no access to your files, your settings,
your encryption keys, or any other information about your use of this
software.

The only outbound network activity this application performs is FTP
transfers to the server address you configure yourself.`

	closeBtn := widget.NewButton("Close", func() { w.Close(); a.Quit() })
	closeBtn.Importance = widget.HighImportance

	content := container.NewScroll(container.NewVBox(
		widget.NewLabelWithStyle("Freezer  v"+appVersion, fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Client-side cold storage with FTP archiving and AES-256-GCM encryption."),
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Disclaimer", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel(disclaimer),
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Privacy Policy", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel(privacy),
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), closeBtn, layout.NewSpacer()),
	))

	w.SetContent(content)
	w.Show()
	a.Run()
}

// launchPassphrasePrompt opens a small Fyne window asking for the encryption passphrase.
// The derived key is stored in memory only for the current session.
func launchPassphrasePrompt() {
	a := app.NewWithID("com.freezer.unlock")
	a.Settings().SetTheme(compactTheme{Theme: theme.DefaultTheme()})
	w := a.NewWindow("Unlock Encryption")
	w.Resize(fyne.NewSize(400, 180))
	w.SetFixedSize(true)
	w.SetCloseIntercept(func() { w.Close(); a.Quit() })

	entry := widget.NewPasswordEntry()
	entry.SetPlaceHolder("Enter passphrase")
	status := widget.NewLabel("")

	okBtn := widget.NewButton("Unlock", func() {
		p := strings.TrimSpace(entry.Text)
		if p == "" {
			status.SetText("Passphrase cannot be empty.")
			return
		}
		key, err := deriveKey(p, appConfig.EncryptionSalt)
		if err != nil {
			status.SetText("Error: " + err.Error())
			return
		}
		activeEncryptionKey = key
		log.Printf("Encryption unlocked for this session")
		w.Close()
		a.Quit()
	})
	okBtn.Importance = widget.HighImportance

	cancelBtn := widget.NewButton("Cancel", func() { w.Close(); a.Quit() })

	w.SetContent(container.NewVBox(
		widget.NewLabel("Enter your encryption passphrase to enable encrypted sync for this session."),
		entry,
		status,
		container.NewHBox(layout.NewSpacer(), okBtn, cancelBtn),
	))
	w.Show()
	a.Run()
}

func onReady() {
	setTrayIcon()
	systray.SetTitle("❄️")
	systray.SetTooltip("Freezer sync")

	statusItem := systray.AddMenuItem("Local: "+appConfig.Root, "Current local sync folder")
	statusItem.Disable()
	remoteStatusItem := systray.AddMenuItem("FTP root: "+appConfig.FTPRoot, "Current FTP root folder")
	remoteStatusItem.Disable()
	batteryStatusItem := systray.AddMenuItem("Sync on battery: "+strconv.FormatBool(appConfig.SyncOnBattery), "Whether background sync is allowed while on battery")
	batteryStatusItem.Disable()
	sleepStatusItem := systray.AddMenuItem("Prevent sleep during sync: "+strconv.FormatBool(appConfig.PreventSleepDuringSync), "Whether to prevent machine sleep during sync")
	sleepStatusItem.Disable()
	systray.AddSeparator()
	settingsItem := systray.AddMenuItem("Settings...", "Edit FTP and local folder configuration")
	unlockItem := systray.AddMenuItem("Unlock encryption...", "Enter passphrase to unlock encrypted sync for this session")
	if !appConfig.EncryptionEnabled {
		unlockItem.Hide()
	}
	syncItem := systray.AddMenuItem("Sync now", "Run an immediate sync")
	restoreItem := systray.AddMenuItem("Restore placeholders", "Download archived files back to disk")
	openFolderItem := systray.AddMenuItem("Open archive folder", "Open the local cold-storage folder")
	systray.AddSeparator()
	aboutItem := systray.AddMenuItem("About Freezer", "Version info, privacy policy, and disclaimer")
	quitItem := systray.AddMenuItem("Quit", "Exit the app")

	go func() {
		for {
			select {
			case <-settingsItem.ClickedCh:
				go openSettingsWindow()
			case <-unlockItem.ClickedCh:
				go launchPassphrasePrompt()
			case <-syncItem.ClickedCh:
				refreshConfig()
				if !shouldSyncNow() {
					log.Printf("sync skipped: machine is on battery and sync-on-battery is disabled")
					continue
				}
				if err := runColdStorage(appConfig.Root, appConfig.Host, appConfig.User, appConfig.Pass); err != nil {
					log.Printf("sync failed: %v", err)
				}
			case <-restoreItem.ClickedCh:
				refreshConfig()
				if err := restorePlaceholderFiles(appConfig.Root, appConfig.Host, appConfig.User, appConfig.Pass); err != nil {
					log.Printf("restore failed: %v", err)
				}
			case <-openFolderItem.ClickedCh:
				openFolder(appConfig.Root)
			case <-aboutItem.ClickedCh:
				go showAboutWindow()
			case <-quitItem.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()

	go func() {
		for range time.Tick(time.Minute) {
			refreshConfig()
			if !shouldSyncNow() {
				continue
			}
			if !isScheduledSyncWindow(time.Now()) {
				continue
			}
			if err := runColdStorage(appConfig.Root, appConfig.Host, appConfig.User, appConfig.Pass); err != nil {
				log.Printf("scheduled sync failed: %v", err)
			}
		}
	}()
}

func onExit() {}

func main() {
	// Write logs to a file so startup errors are always visible on Windows
	logDir, _ := os.UserCacheDir()
	logPath := filepath.Join(logDir, "freezer", "freezer.log")
	os.MkdirAll(filepath.Dir(logPath), 0755)
	if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, logFile))
		defer logFile.Close()
	}
	log.Printf("Freezer starting (os=%s)", runtime.GOOS)

	appConfig = loadConfig()
	if restorePath != "" {
		log.Printf("Restoring single file: %s", restorePath)
		if err := restoreSingleFile(restorePath); err != nil {
			log.Printf("restore failed: %v", err)
			os.Exit(1)
		}
		log.Printf("Restored successfully")
		return
	}
	if settingsMode {
		log.Println("Opening settings window")
		runSettingsWindow()
		return
	}
	log.Printf("Tray app starting in %s", appConfig.Root)
	systray.Run(onReady, onExit)
}
