package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/getlantern/systray"
)

const defaultRetentionDays = 60
const metadataDirName = ".coldstorage"

// appVersion is set at build time via: -ldflags "-X main.appVersion=$(git describe --tags --always --dirty)"
var appVersion = "dev"

var appConfig Config
var settingsMode bool
var restorePath string
var aboutMode bool

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
	if aboutMode {
		showAboutWindow()
		return
	}
	log.Printf("Tray app starting in %s", appConfig.Root)
	systray.Run(onReady, onExit)
}
