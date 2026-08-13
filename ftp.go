package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

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

// FTPCapabilities describes which integrity commands the FTP server supports.
type FTPCapabilities struct {
	SIZE    bool
	HASH    bool
	XMD5    bool
	XCRC    bool
	XSHA256 bool
}

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

func probeFTPCapabilities(host, user, pass string) (FTPCapabilities, error) {
	caps := FTPCapabilities{SIZE: true}

	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return caps, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))

	r := bufio.NewReader(conn)
	readFTPResponse(r)

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

	appConfig.LastIndexBackup = time.Now()
	_ = saveConfig(appConfig)
	return nil
}

func measureFTPThroughput(host, user, pass, ftpRoot string) (uploadMbps float64, downloadMbps float64, err error) {
	uploadClient, err := connectFTP(host, user, pass)
	if err != nil {
		return 0, 0, err
	}
	defer uploadClient.Quit()

	testName := ".freezer-test.bin"
	remotePath := remotePathFromLocal(testName, ftpRoot)
	payload := bytes.Repeat([]byte("X"), 10485760)
	payloadSize := len(payload)

	uploadStart := time.Now()
	if err := uploadClient.Stor(remotePath, bytes.NewReader(payload)); err != nil {
		return 0, 0, err
	}
	uploadElapsed := time.Since(uploadStart).Seconds()
	if uploadElapsed <= 0 {
		uploadElapsed = 0.001
	}
	uploadMbps = (float64(payloadSize) * 8 / 1000000) / uploadElapsed

	time.Sleep(100 * time.Millisecond)

	downloadClient, err := connectFTP(host, user, pass)
	if err != nil {
		return uploadMbps, 0, err
	}
	defer downloadClient.Quit()

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

	downloadClient.Delete(remotePath)
	return uploadMbps, downloadMbps, nil
}
