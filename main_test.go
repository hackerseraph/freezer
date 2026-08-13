package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordExpiryWindow(t *testing.T) {
	now := time.Now()
	rec := Record{ExpiresAt: now.Add(60 * 24 * time.Hour)}
	if rec.IsExpired(now.Add(30 * 24 * time.Hour)) {
		t.Fatal("expected file to be active before the 60-day retention window ends")
	}
	if !rec.IsExpired(now.Add(61 * 24 * time.Hour)) {
		t.Fatal("expected file to be expired after the 60-day retention window")
	}
}

func TestPlaceholderHandling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.txt")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	rec := Record{
		LocalPath:  path,
		RemotePath: "/remote/example.txt",
		ExpiresAt:  time.Now().Add(retentionDuration()),
	}
	if err := archiveLocalFile(path, rec); err != nil {
		t.Fatal(err)
	}

	// Original file should be gone
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expected original file to be removed after archiving")
	}

	// .frozen stub should exist and contain content
	frozenPath := path + ".frozen"
	info, err := os.Stat(frozenPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("expected .frozen stub to have content, got empty file")
	}
}

func TestConfigPersistence(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "settings.json")
	cfg := Config{
		Root:          "/tmp/archive",
		Host:          "ftp.example.com:21",
		User:          "demo-user",
		Pass:          "demo-pass",
		FTPRoot:       "/coldstorage",
		SyncOnBattery: true,
		Interval:      10 * time.Minute,
	}

	if err := saveConfigToFile(cfg, cfgPath); err != nil {
		t.Fatal(err)
	}

	loaded, err := readConfigFromFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Root != cfg.Root || loaded.Host != cfg.Host || loaded.User != cfg.User || loaded.Pass != cfg.Pass || loaded.FTPRoot != cfg.FTPRoot || loaded.SyncOnBattery != cfg.SyncOnBattery || loaded.Interval != cfg.Interval {
		t.Fatalf("config did not persist correctly: %#v", loaded)
	}
}

func TestDefaultSyncOnBatteryDisabled(t *testing.T) {
	cfg := defaultConfig()
	if cfg.SyncOnBattery {
		t.Fatal("expected sync on battery to default to false")
	}
}

func TestRemotePathUsesConfiguredRemoteRoot(t *testing.T) {
	remotePath := remotePathFromLocal("nested/file.txt", "/coldstorage")
	if remotePath != "/coldstorage/nested/file.txt" {
		t.Fatalf("expected /coldstorage/nested/file.txt, got %s", remotePath)
	}

	remotePath = remotePathFromLocal("nested/file.txt", "/")
	if remotePath != "/nested/file.txt" {
		t.Fatalf("expected /nested/file.txt, got %s", remotePath)
	}
}

func TestEncryptionRoundTrip(t *testing.T) {
	salt, err := newEncryptionSalt()
	if err != nil {
		t.Fatal(err)
	}
	key, err := deriveKey("test-passphrase", salt)
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("Hello, Freezer encryption test!")
	ciphertext, err := encryptStream(bytes.NewReader(plaintext), key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext should not equal plaintext")
	}

	recovered, err := decryptBytes(ciphertext, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recovered, plaintext) {
		t.Fatalf("decrypted content does not match original: got %q want %q", recovered, plaintext)
	}
}

func TestEncryptionWrongKeyFails(t *testing.T) {
	salt, _ := newEncryptionSalt()
	key1, _ := deriveKey("correct-passphrase", salt)
	key2, _ := deriveKey("wrong-passphrase", salt)

	ciphertext, err := encryptStream(bytes.NewReader([]byte("secret")), key1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptBytes(ciphertext, key2); err == nil {
		t.Fatal("expected decryption with wrong key to fail")
	}
}
