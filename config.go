package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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

// retentionDays returns the configured retention period, falling back to the default.
func retentionDuration() time.Duration {
	days := appConfig.RetentionDays
	if days <= 0 {
		days = defaultRetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
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
	flag.BoolVar(&aboutMode, "about", false, "open the about window")
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
