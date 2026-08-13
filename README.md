# freezer
A small Go project for using external storage as cold storage. Centered around laptops and environments with fast but small local storage, this lets you automatically move uncommon files to cold storage, to keep a constant churn of free space on your local devices.

## Contents

- [Installation](#installation)
  - [Debian / Ubuntu / Linux Mint](#debian--ubuntu--linux-mint)
  - [Windows 10 / 11](#windows-10--11)
- [Run](#run)
- [Test](#test)
- [Technical Architecture](#technical-architecture)
  - [Design Philosophy](#design-philosophy)
  - [The State Index](#the-state-index)
  - [The File Lifecycle](#the-file-lifecycle)
  - [Sync Phase](#sync-phase)
  - [Prune Phase](#prune-phase)
  - [Restore Phase](#restore-phase)
  - [Retention and the Expiry Clock](#retention-and-the-expiry-clock)
  - [File Change Detection](#file-change-detection)
  - [FTP as a Dumb Store](#ftp-as-a-dumb-store)
  - [Multi-Machine Behaviour](#multi-machine-behaviour)
  - [State Loss and Recovery](#state-loss-and-recovery)
  - [Security Considerations](#security-considerations)
- [How Record Keeping Works](#how-record-keeping-works)
  - [The Record Structure](#the-record-structure)
  - [How Sync Uses the Record](#how-sync-uses-the-record)
  - [File Integrity and FTP Limitations](#file-integrity-and-ftp-limitations)
- [Understanding Shell Integration](#understanding-shell-integration)
  - [Ubuntu / Linux Mint / Debian](#ubuntu--linux-mint--debian)
  - [Windows 10 / 11](#windows-10--11-1)
- [Current Features](#current-features)
  - [Archive and Sync](#archive-and-sync)
  - [File Integrity Verification](#file-integrity-verification)
  - [Restore](#restore)
  - [Settings Panels](#settings-panels)
  - [Platform](#platform)
- [Screenshots](#screenshots)
- [Planned Features](#planned-features)
  - [File Integrity](#file-integrity)
  - [Multi-User and Access Control](#multi-user-and-access-control)
  - [Security](#security)
  - [Storage Backends](#storage-backends)
  - [Migration and Uninstall](#migration-and-uninstall)
  - [Scheduling](#scheduling)
  - [Uninstall and Data Recovery](#uninstall-and-data-recovery)

## Technical Architecture

### Design Philosophy

Freezer is a fully client-side cold storage manager. There is no server component, no daemon running on the FTP host, and no central coordination service. Every decision about what to archive, when to archive it, and when to restore it is made entirely on the local machine using a bbolt embedded database. The FTP server is treated as a dumb block store: it holds bytes and returns them on request. It knows nothing about expiry dates, file relationships, or Freezer itself.

This design means Freezer is simple to deploy, requires no special software on the server side, and works with any standard FTP endpoint including NAS devices, shared hosting, and home servers. The trade-off is that all state is local and there is no built-in mechanism for two machines to share a consistent view of the archive.

### The State Index

All tracking is done through a bbolt embedded B-tree database at `.coldstorage/index.db` inside your configured root folder. bbolt is a pure-Go, zero-CGo, single-file embedded database. It stores records as B-tree nodes, provides O(log n) key lookups, and writes each record change individually rather than rewriting the entire file. This makes it fast and efficient even with hundreds of thousands of tracked files.

The database is opened once per sync cycle and closed when the cycle completes. Each record write is its own ACID transaction, so a crash mid-sync leaves the database consistent rather than corrupt.

If a legacy `index.json` file is found on first open and the database is empty, records are automatically migrated and the old file is renamed to `index.json.migrated`.

Each entry in the index is keyed by the absolute local path of the file and contains:

| Field | Type | Purpose |
|---|---|---|
| `local_path` | string | Absolute path to the file on local disk |
| `remote_path` | string | Full path on the FTP server |
| `uploaded_at` | timestamp | When the last successful upload completed |
| `expires_at` | timestamp | When the local copy is scheduled to be archived |
| `placeholder` | bool | True if the file is currently a .frozen stub |
| `content_hash` | string | SHA-256 of the file content at upload time |
| `last_modified` | timestamp | File modification time recorded at upload |
| `file_size` | int64 | File size in bytes recorded at upload |

The index is updated record-by-record as files are processed. Individual record writes are committed immediately as ACID transactions. No full file rewrite occurs.

### The File Lifecycle

A file moves through the following states:

1. **Untracked** — the file exists on disk but has no entry in the index. This is the initial state for all files before Freezer first runs.
2. **Tracked and warm** — the file has been uploaded to FTP. The local copy is intact and `placeholder` is false. The expiry clock is running.
3. **Expired and pending prune** — `expires_at` has passed on the next prune run. The file is still present locally but will be archived on the next sync cycle.
4. **Frozen** — the local file has been removed and replaced with a `.frozen` stub. `placeholder` is true. The content lives only on the FTP server.
5. **Restored** — the file has been downloaded from FTP back to its original path. The stub is deleted. `placeholder` is set back to false and the expiry clock resets from the moment of restore.

### Sync Phase

On every sync cycle, `syncFolder` performs a depth-first walk of the root folder. For each regular file it encounters (skipping the `.coldstorage` directory, `.frozen` stubs, and any excluded subfolders) it applies the following logic:

- If there is no record in the index, upload the file and create a record with `expires_at = now + retention_days`.
- If the file's modification time on disk is newer than the `last_modified` value in the record, re-upload the file and reset `expires_at`.
- If `expires_at` has already passed and the file is still on disk, re-upload and reset the expiry clock. (This handles the case where the prune phase has not yet run.)
- If the record exists, modification time matches, and expiry has not passed, skip the file entirely. No FTP connection is made.

The FTP server is only contacted when a file actually needs to be uploaded. Unchanged files within their retention window are evaluated purely against the local index with no network activity.

### Prune Phase

After the sync phase completes, `pruneExpired` iterates all records in the index and identifies files where `placeholder` is false and `expires_at` is in the past. For each such file:

1. A `SIZE` command is sent to the FTP server to confirm the remote file exists and is non-zero. If this fails, the file is skipped and a warning is logged. The local copy is never deleted if the remote copy cannot be confirmed.
2. If size verification is enabled, the remote size returned by `SIZE` is compared against the `file_size` field in the record. A mismatch indicates a truncated or corrupt upload and the file is skipped.
3. If hash verification is enabled and the server supports it (via `HASH`, `XMD5`, `XCRC`, or `XSHA256`), the remote hash is retrieved and compared against the stored `content_hash`. A mismatch skips the file.
4. If all checks pass, the local file is deleted and a `.frozen` stub is written in its place. The stub is a plain text file containing the original filename, the archive date, the remote path, and instructions for restoring.
5. The record is updated with `placeholder: true` and the index is saved.

The local file is never deleted unless the remote copy has been positively confirmed to exist. If no verification method is available and the user has not enabled the "allow unsafe freeze" option, the file is skipped rather than archived blindly.

### Restore Phase

Restoring a file reverses the prune: the content is downloaded from `remote_path`, written to `local_path`, the `.frozen` stub is deleted, `placeholder` is set to false, and `expires_at` is reset to `now + retention_days`. The file re-enters the normal warm lifecycle from that point.

Restoration can be triggered three ways:

- **Restore all** from the tray menu runs `restorePlaceholderFiles`, which iterates all records with `placeholder: true` and restores each one.
- **Double-click a .frozen stub** (after shell integration is installed) calls `freezer -restore /path/to/file.ext.frozen`, which looks up the record for the original path, connects to FTP, and restores only that file.
- The internal `restorePlaceholderIfNeeded` function is available for programmatic use in future features such as on-demand restore before file access.

### Retention and the Expiry Clock

The retention period (configurable via the Storage slider, default 60 days) defines how long a file stays warm on local disk after its most recent upload. The clock starts at the moment of upload, not at the file's creation or modification time.

This means a file written to disk 5 years ago but uploaded to Freezer for the first time today will expire 60 days from today, not 5 years ago. The local modification time is recorded in the index and used only to detect changes, not to calculate expiry.

When a file is modified and re-uploaded, the expiry clock resets from the new upload time. A frequently edited file will never be archived as long as it continues to change within the retention window.

### File Change Detection

Freezer uses the file system modification time (`mtime`) as the primary signal for change detection. If `mtime` on disk is newer than the `last_modified` value stored in the index, the file is considered changed and re-uploaded regardless of content.

A SHA-256 hash is also computed and stored at upload time. This hash is currently used for remote integrity verification (confirming the FTP copy matches what was sent) but is not used for local change detection. A future improvement would use the hash to skip re-uploads when `mtime` has changed but content has not (for example after a copy operation that updates timestamps without changing bytes).

### FTP as a Dumb Store

The FTP server requires no special configuration and runs no Freezer-specific software. Freezer uses a small subset of the FTP protocol:

- `USER` / `PASS` for authentication
- `CWD` / `MKD` for navigating and creating directories
- `STOR` for uploading files
- `RETR` for downloading files
- `SIZE` for verifying file existence and size before archiving
- `DELE` for cleaning up temporary throughput test files
- `FEAT` for probing optional integrity extensions (HASH, XMD5, XCRC, XSHA256)

The server holds files at the paths Freezer assigns. It has no awareness of expiry dates, placeholder status, or any other metadata. If you browse the FTP server directly you will see the archived files in the same folder structure as your local root, with no special markers.

### Multi-Machine Behaviour

Because all state is local, two machines pointing at the same FTP root folder operate completely independently. Each machine has its own `index.db` and its own expiry clocks. Machine A archiving a file does not cause Machine B to see it as archived, and Machine B restoring a file does not affect Machine A's records.

This is intentional for single-user use across multiple machines: each machine manages its own local disk independently. The FTP server acts as a shared pool of archived content but not as a coordination layer.

For multi-user shared archives, this architecture requires extension. File locking, shared state, and access control are listed in the planned features section.

### Index Backup

After each sync cycle, Freezer checks whether it has been more than 7 days since the last index backup. If so, it uploads a copy of `index.db` to `<ftp-root>/.freezer-backups/index-<hostname>-<date>.db` on the FTP server. The hostname is included in the filename so that multiple machines backing up to the same FTP root do not overwrite each other's backups.

If the local index is lost or corrupted, you can recover by downloading the most recent backup from FTP and placing it at `.coldstorage/index.db` in your root folder. Freezer will resume from that point with no re-upload of files that were already archived.

### State Loss and Recovery

If `.coldstorage/index.db` is deleted or corrupted, Freezer loses all memory of what is archived. On the next sync it will attempt to re-upload every file in the root folder as if seeing them for the first time. Files that are currently frozen (`.frozen` stubs present on disk) will not be uploaded because they end in `.frozen` and are skipped by the walker. Their content remains on the FTP server but there will be no record pointing to it.

To recover from state loss when frozen files are present:
1. Do not run a sync until the index is rebuilt or restored from backup.
2. Download the most recent backup from FTP at `.freezer-backups/index-<hostname>-<date>.db` and place it at `.coldstorage/index.db` in your root folder.
3. If no backup exists, you can manually restore frozen files by downloading them from the FTP server and recreating records, or by using "Restore all" from the tray if any index entries survive.

Weekly backups are uploaded automatically to your FTP server. Keep at least one recent backup accessible in case of local disk failure.

### Security Considerations

FTP transmits credentials and file content in plaintext. Anyone with access to the network path between the client and server can intercept both. For use over untrusted networks, a VPN or SSH tunnel should wrap the FTP connection.

Credentials are stored in the Freezer config file at `~/.config/coldstore/settings.json` (Linux) or `%APPDATA%\coldstore\settings.json` (Windows) with filesystem permissions set to the owning user only (0600 on Linux). They are not encrypted at rest.

The `.frozen` stub files contain the remote path of the archived content. Anyone with access to the stub can determine where the file is stored on the FTP server. The stub does not contain credentials.

Host-side encryption (encrypting content before upload so the FTP server holds only ciphertext) is listed in the planned features section.

- [Privacy Policy](#privacy-policy)
- [Disclaimer](#disclaimer)

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

## How Record Keeping Works

Freezer maintains a state index at `.coldstorage/index.db` inside your root folder. This bbolt database is the source of truth for everything Freezer knows about your archived files. It functions like an inode map: the full local path of each file is the key, and the value is a record containing all the metadata Freezer needs to manage that file.

### The Record Structure

Each tracked file has a record with the following fields:

- `local_path` - the absolute path where the file lives (or lived) on disk
- `remote_path` - the full path on the FTP server where the content is stored
- `uploaded_at` - timestamp of the last successful upload
- `expires_at` - when the local copy is scheduled to be replaced with a .frozen stub
- `placeholder` - true if the file has been archived and only a .frozen stub exists locally
- `content_hash` - SHA-256 hash of the file content at the time of upload
- `last_modified` - the file's modification time on disk at the time of upload

### How Sync Uses the Record

When Freezer syncs, it walks your root folder and checks each file against the index. A file is uploaded if there is no record for it, if the local modification time is newer than what was recorded, or if the expiry has passed. After a successful upload the record is written with the new hash, timestamps, and expiry date.

When the expiry date passes, Freezer checks that the file exists on the FTP server and then replaces the local file with a `.frozen` stub. The record is updated to mark `placeholder: true`. The original content remains on FTP indefinitely.

When you restore a file, Freezer downloads from the remote path in the record, writes the content back to the original local path, deletes the `.frozen` stub, and resets the expiry clock.

### File Integrity and FTP Limitations

At upload time Freezer computes a SHA-256 hash of the local file and stores it in the record. This hash is used to detect whether a file has changed since it was last uploaded.

However, verifying that the file on the FTP server has the same hash is a different problem. Standard FTP (RFC 959) has no hash or checksum command. The only standard way to check the remote file is to download it again and hash it locally, which would double the bandwidth cost of every archive operation.

Some FTP servers support optional extensions like `XCRC`, `XSHA1`, `XSHA256`, or the RFC 3659 `HASH` command, but these are not guaranteed to be present and a plain FTP server will not have them.

What Freezer currently does before archiving a local file is call the standard `SIZE` command to confirm the remote file exists and is non-zero. This catches missing or empty files but will not catch a truncated upload where the server accepted a partial write without error.

Adding optional hash verification via the extended `HASH` command (falling back to size-only when the server does not support it) is listed as a planned improvement.

## Understanding Shell Integration

Freezer replaces archived files with `.frozen` stub files so you can see what has been moved to cold storage without losing the file name or folder structure. Shell integration teaches the operating system to open those stubs with Freezer when you double-click them, triggering an automatic restore from your FTP server.

### Ubuntu / Linux Mint / Debian

Clicking "Install shell integration" in Settings > System does the following, all within your home directory with no root access required:

Writes a MIME type definition to `~/.local/share/mime/packages/freezer.xml` that tells the OS `.frozen` files belong to the type `application/x-freezer-placeholder`. Runs `update-mime-database` to register it.

Writes a desktop handler to `~/.local/share/applications/freezer-restore.desktop` that tells any XDG-compliant file manager (Thunar, Nautilus, Nemo, Dolphin) to run `freezer -restore %f` when a file of that type is opened. Runs `update-desktop-database` to register it.

Calls `xdg-mime default freezer-restore.desktop application/x-freezer-placeholder` to set Freezer as the default handler for that MIME type.

The result is that double-clicking a `.frozen` file in any supported file manager runs `freezer -restore /full/path/to/file.ext.frozen`, which connects to FTP, downloads the original content, and replaces the stub with the real file.

To uninstall: delete `~/.local/share/mime/packages/freezer.xml` and `~/.local/share/applications/freezer-restore.desktop`, then run `update-mime-database ~/.local/share/mime` and `update-desktop-database ~/.local/share/applications`.

### Windows 10 / 11

Clicking "Install shell integration" in Settings > System writes four registry keys under `HKEY_CURRENT_USER` using `reg add`. Because it targets the current user hive rather than `HKLM`, no UAC prompt or administrator rights are required.

```
HKCU\Software\Classes\.frozen
  (Default) = FreezerPlaceholder

HKCU\Software\Classes\FreezerPlaceholder
  (Default) = Freezer Archive File

HKCU\Software\Classes\FreezerPlaceholder\DefaultIcon
  (Default) = C:\path\to\freezer.exe,0

HKCU\Software\Classes\FreezerPlaceholder\shell\open\command
  (Default) = "C:\path\to\freezer.exe" -restore "%1"
```

The path to `freezer.exe` is resolved at runtime so it points to wherever the binary actually lives. The result is that double-clicking any `.frozen` file in Windows Explorer runs `freezer.exe -restore "C:\path\to\file.ext.frozen"`, which connects to FTP, downloads the original content, and replaces the stub with the real file.

To uninstall: delete the `HKCU\Software\Classes\.frozen` and `HKCU\Software\Classes\FreezerPlaceholder` keys from the registry.

## Current Features

### Archive and Sync
- Files in the configured root folder are uploaded to an FTP server and tracked with SHA-256 content hashes and file size
- Configurable retention period (7 to 365 days) set via slider in Storage Settings; default is 60 days
- Files past their retention date are replaced with a `.frozen` stub on disk; content remains on FTP
- Modified files are automatically detected by modification time and re-uploaded; expiry clock resets
- Subfolders can be excluded from sync via checkboxes in the Exclusions panel

### File Integrity Verification
- Before archiving a local file, Freezer optionally verifies the remote copy using:
  - SIZE command: confirms the remote file size matches the locally recorded size (works on all FTP servers)
  - Hash command: confirms content via HASH, XSHA256, XMD5, or XCRC if the server supports it
- FTP server capabilities are discovered by clicking "Probe server capabilities" in FTP settings, which sends FEAT and auto-enables the best available verification method
- An "allow unsafe freeze" option lets users archive even when no verification is available, with explicit acknowledgement of the risk
- Files that fail verification are skipped and logged rather than silently archived

### Restore
- All placeholders can be restored at once from the tray menu
- Individual files can be restored by double-clicking the `.frozen` stub after running shell integration install
- Shell integration registers `.frozen` files with the OS on both Windows and Linux

### Settings Panels
- Power: sync on battery toggle, prevent sleep during sync (systemd-inhibit on Linux, Windows API on Windows)
- Sync Schedule: weekday selection for nightly scheduled sync
- FTP and Folder: server, credentials, FTP root, local folder browser with sidebar/breadcrumbs/history, FTP throughput test with progress spinner, server capability probe and integrity verification options
- Exclusions: checkboxes for each subfolder in the root folder, auto-populated when root changes
- Storage: retention period slider (7 to 365 days) with labelled tick marks
- System: install shell integration for double-click restore on Linux and Windows

### Platform
- Runs on Windows 10/11 and Linux (Debian, Ubuntu, Mint, and compatible distributions)
- System tray icon with sync now, restore, open folder, and quit options
- Startup log written to the OS cache directory for diagnosing silent crashes

## Screenshots

### Power
![Power settings](screenshots/settings-power.png)

### Sync Schedule
![Sync Schedule settings](screenshots/settings-sync-schedule.png)

### FTP & Folder
![FTP and Folder settings](screenshots/settings-ftp-folder.png)

### Exclusions
![Exclusions settings](screenshots/settings-exclusions.png)

### Storage
![Storage settings](screenshots/settings-storage.png)

### System Integration
![System Integration settings](screenshots/settings-system.png)

## Planned Features

### File Integrity
- Content hashing upgrade: migrate from SHA-256 to MD5+SHA-256 dual hashing for faster change detection on large files while retaining collision resistance for archival verification
- File fingerprinting: maintain an inode/fingerprint index alongside the archive state so renamed or moved files are recognised as the same file rather than re-uploaded as new

### Compression
- Pre-upload compression: compress file content before encrypting and uploading to reduce FTP storage usage and transfer time; candidates are zstd (fast, high ratio) and gzip (universal compatibility)
- Magic byte detection: read the first bytes of each file before compressing and compare against a table of known compressed format signatures (JPEG, PNG, MP4, ZIP, GZ, ZSTD, etc.); skip compression automatically for files that are already compressed to avoid wasted CPU and potentially larger output
- Compression level setting: expose a slider in Storage Settings to let users trade transfer speed against storage savings
- Compressed file format: extend the Freezer file envelope (FRZR magic header) with a compression flag so the correct decompression is applied on restore regardless of when the file was archived

### Multi-User and Access Control
- File locking: advisory locks to prevent simultaneous archive/restore conflicts when multiple users or machines share the same FTP root
- Users and groups: per-user configuration profiles and group-based access policies so shared deployments can restrict which folders each user can archive or restore
- Admin roles: privilege tiers (admin / operator / read-only) with audit logging of all archive and restore actions

### Security
- Host-side encryption: encrypt file content before upload so the FTP server holds only ciphertext; decryption happens locally on restore
- Shared key management: distribute symmetric keys to authorised clients without embedding credentials in config files
- Certificate-based auth: WireGuard-style keypair authentication for FTP sessions, eliminating plaintext username/password credentials in settings
- Virus scanning: integrate ClamAV to scan files on restore before writing them back to disk, blocking any malware that may have been uploaded by another client

### Storage Backends
- External drives: archive to a locally attached USB or eSATA drive instead of a network server, with automatic detection when the drive is connected and graceful handling when it is not
- FTP: current default backend (already implemented)
- NFS shares: mount and write directly to NFS exports, suitable for home lab NAS devices and Linux servers
- CIFS/SMB shares: support Windows file shares and Samba, covering NAS appliances and Windows Server environments
- Cloud storage: Google Drive, OneDrive, and Dropbox as archive targets via their respective APIs, selectable from the settings UI alongside FTP and local network options
- Pluggable backend interface: common abstraction layer so additional backends can be added without changes to the core sync logic

### Migration and Uninstall
- Migrate tray menu item: move all archived content from the current backend to a new one (e.g. FTP to Google Drive) without losing state, rewriting remote paths in the index as transfers complete
- Uninstall tray menu item: rewarm all frozen files back to local disk, remove all .frozen stubs and the .coldstorage metadata directory, deregister shell integration, and optionally delete archived content from the remote backend, leaving the system in a clean pre-Freezer state
- Pre-uninstall disk space check: calculate total rewarm size before starting and warn the user if there is not enough local space, offering the date-range partial restore slider if needed
- Migration log: write a persistent log of every file transferred during a migration so that if the process is interrupted by a power outage or crash it can resume from where it left off rather than starting over, and any partially transferred files can be detected and retried

### Scheduling
- Flexible schedules: cron-style scheduling beyond the current nightly fixed time, with per-folder schedule overrides
- Nightly cleanup routine: detect and resolve interrupted freeze operations left in a partial state (original file deleted but .frozen stub missing, or stub present with no state record) and reconcile them against the FTP server automatically

### Uninstall and Data Recovery
- Graceful uninstall routine: before removal, rewarm all frozen files from FTP back to local disk and remove all .frozen stubs and the .coldstorage metadata directory, leaving the folder in a clean pre-Freezer state
- Disk space check before rewarm: before restoring any file, verify available local disk space against the total size of all archived content and warn the user if there is not enough room
- Partial rewarm with priority ordering: when there is insufficient space to restore everything, allow the user to choose which files or folders to restore first, or restore by most-recently-archived order until the disk is full
- Date-range rewarm slider: when disk space is limited, show a slider letting the user select how far back to rewarm (e.g. last 7 days, last 30 days). Tick marks on the slider correspond to real archive dates, and the UI calculates and displays the cumulative size that would be restored at each position so the user can find the cutoff that fits their available space before committing
- Rewarm progress reporting: show per-file and overall progress during a full restore so the user knows how long the operation will take and which files have been recovered

## Privacy Policy

Freezer collects no data about you, your files, or your usage.

- No telemetry
- No analytics
- No crash reporting
- No usage statistics
- No network connections except to your own configured FTP server
- No third-party services of any kind

All data processed by Freezer stays on your local machine and your own FTP server. The developers have no access to your files, your settings, your encryption keys, or any other information about your use of this software.

The only outbound network activity this application performs is FTP transfers to the server address you configure yourself.

## Disclaimer

Freezer is provided "as is", without warranty of any kind, express or implied, including but not limited to the warranties of merchantability, fitness for a particular purpose, and non-infringement.

In no event shall the authors or copyright holders be liable for any claim, damages, or other liability — whether in an action of contract, tort, or otherwise — arising from, out of, or in connection with the software or the use or other dealings in the software.

This includes but is not limited to: loss of data, loss of encrypted archives due to forgotten passphrases, FTP server outages, file corruption, accidental deletion, interrupted sync operations, or any financial loss resulting from inability to access archived files.

**You are solely responsible for:**

- Maintaining a secure backup of your encryption passphrase. If it is lost, encrypted archives cannot be recovered by anyone.
- Verifying that files have been successfully archived to your FTP server before relying on Freezer as your only copy.
- Ensuring your FTP server is reliable, has sufficient storage, and is accessible when restores are needed.
- Any data that cannot be recovered due to key loss, server failure, misconfiguration, or software bugs.

Use this software at your own risk.
