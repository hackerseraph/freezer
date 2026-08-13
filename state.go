package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	bbolt "go.etcd.io/bbolt"
)

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

	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(recordsBucket)
		return err
	}); err != nil {
		return err
	}

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
