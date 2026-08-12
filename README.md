# freezer
A small Go project for using external storage as cold storage. Centered around laptops and environments with fast but small local storage, this lets you automatically move uncommon files to cold storage, to keep a constant churn of free space on your local devices.

## Installation

### Debian / Ubuntu / Linux Mint

Install build dependencies:

```bash
sudo apt update
sudo apt install -y golang-go gcc \
  libgl1-mesa-dev libwayland-dev libdbus-1-dev \
  libasound2-dev libxxf86vm-dev libayatana-appindicator3-dev
```

Clone and build:

```bash
git clone https://github.com/hackerseraph/freezer.git
cd freezer
go build -o freezer .
```

Run:

```bash
./freezer
```

To open settings on first run:

```bash
./freezer -settings
```

### Windows 10 / 11

Install Go from https://go.dev/dl/ and TDM-GCC from https://jmeubank.github.io/tdm-gcc/ (required for CGo). Restart your terminal after both installs so PATH updates take effect.

Clone and build:

```powershell
git clone https://github.com/hackerseraph/freezer.git
cd freezer
go build -o freezer.exe .
```

Run:

```powershell
.\freezer.exe
```

To open settings on first run:

```powershell
.\freezer.exe -settings
```

The app runs as a system tray icon. Look for it in the notification area (bottom right, click the ^ arrow if it is not visible).

## Run
go run .

## Test
go test ./...

## Current Features

### Archive and Sync
- Files in the configured root folder are uploaded to an FTP server and tracked with SHA-256 content hashes
- Files older than 60 days are replaced with a `.frozen` stub file on disk; the content lives on FTP
- Modified files are automatically re-uploaded and their expiry resets
- Subfolders can be excluded from sync via the settings UI

### Restore
- All placeholders can be restored at once from the tray menu
- Individual files can be restored by double-clicking the `.frozen` stub after running shell integration install
- Shell integration registers `.frozen` files with the OS on both Windows and Linux

### Settings UI
- Local root folder browser (Thunar/Nautilus style with sidebar, breadcrumbs, back/forward navigation)
- FTP server, username, password, and remote root configuration
- Sync schedule by weekday
- Power settings: sync on battery toggle, prevent sleep during sync
- FTP throughput test with live progress indicator
- Folder exclusion checkboxes auto-populated from the root folder
- Shell integration installer

### Platform
- Runs on Windows and Linux
- System tray icon with sync now, restore, open folder, and quit options
- Cross-platform sleep prevention during sync (systemd-inhibit on Linux, Windows API on Windows)

## Planned Features

### File Integrity
- Content hashing upgrade: migrate from SHA-256 to MD5+SHA-256 dual hashing for faster change detection on large files while retaining collision resistance for archival verification
- File fingerprinting: maintain an inode/fingerprint index alongside the archive state so renamed or moved files are recognised as the same file rather than re-uploaded as new

### Multi-User and Access Control
- File locking: advisory locks to prevent simultaneous archive/restore conflicts when multiple users or machines share the same FTP root
- Users and groups: per-user configuration profiles and group-based access policies so shared deployments can restrict which folders each user can archive or restore
- Admin roles: privilege tiers (admin / operator / read-only) with audit logging of all archive and restore actions

### Security
- Host-side encryption: encrypt file content before upload so the FTP server holds only ciphertext; decryption happens locally on restore
- Shared key management: distribute symmetric keys to authorised clients without embedding credentials in config files
- Certificate-based auth: WireGuard-style keypair authentication for FTP sessions, eliminating plaintext username/password credentials in settings
- Virus scanning: integrate ClamAV to scan files on restore before writing them back to disk, blocking any malware that may have been uploaded by another client

### Scheduling
- Flexible schedules: cron-style scheduling beyond the current nightly fixed time, with per-folder schedule overrides
- Nightly cleanup routine: detect and resolve interrupted freeze operations left in a partial state (original file deleted but .frozen stub missing, or stub present with no state record) and reconcile them against the FTP server automatically
