package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

func syncFolder(root string, client *ftp.ServerConn, state *State) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if filepath.Base(path) == metadataDirName {
				return filepath.SkipDir
			}
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
