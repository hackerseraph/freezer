package main

import (
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

	if err := archiveLocalFile(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected placeholder to be empty, got size %d", info.Size())
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
