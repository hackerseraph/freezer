# freezer
A small Go project for using external storage as cold storage. Centered around laptops and environments with fast but small local storage, this lets you automatically move uncommon files to cold storage, to keep a constant churn of free space on your local devices.

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
