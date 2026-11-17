package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (manager *Manager) acquireFileLock(ctx context.Context) (func(), error) {
	paths := []string{manager.path}
	if manager.authPath != "" && !strings.EqualFold(manager.path, manager.authPath) {
		paths = append(paths, manager.authPath)
	}
	sort.Strings(paths)
	var releases []func()
	releaseAll := func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}
	for _, path := range paths {
		release, err := manager.acquirePathLock(ctx, path)
		if err != nil {
			releaseAll()
			return nil, err
		}
		releases = append(releases, release)
	}
	return releaseAll, nil
}

func (manager *Manager) acquirePathLock(ctx context.Context, path string) (func(), error) {
	lockPath := path + ".refresh.lock"
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, &Error{Code: "temporary", cause: ErrTemporary}
	}
	marker := []byte(hex.EncodeToString(nonce[:]))
	deadline := time.Now().Add(manager.lockTimeout)
	for {
		file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			if _, err = file.Write(marker); err != nil {
				file.Close()
				os.Remove(lockPath)
				return nil, &Error{Code: "temporary", cause: ErrTemporary}
			}
			if err = file.Close(); err != nil {
				os.Remove(lockPath)
				return nil, &Error{Code: "temporary", cause: ErrTemporary}
			}
			return func() {
				contents, err := os.ReadFile(lockPath)
				if err == nil && bytes.Equal(contents, marker) {
					_ = os.Remove(lockPath)
				}
			}, nil
		}
		if !os.IsExist(err) || time.Now().After(deadline) {
			return nil, &Error{Code: "file_conflict", cause: ErrConflict}
		}
		if err := waitContext(ctx, 50*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

func (manager *Manager) replaceCredential(ctx context.Context, source credential, renewed renewal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if source.auth != nil {
		current, err := readSnapshot(manager.path)
		if err != nil || !bytes.Equal(current, source.raw) {
			return &Error{Code: "file_conflict", cause: ErrConflict}
		}
		if err := replaceSnapshot(ctx, manager.authPath, source.auth.raw, source.auth.replaced(renewed.cookies, renewed.token)); err != nil {
			return err
		}
		if strings.EqualFold(manager.path, manager.authPath) {
			return nil
		}
	}
	cookies := renewed.cookies
	if source.auth != nil {
		cookies = nil
		for _, cookie := range renewed.cookies {
			if sessionCookie(cookie.Name) {
				cookies = append(cookies, cookie)
			}
		}
	}
	return replaceSnapshot(ctx, manager.path, source.raw, source.replaced(renewed.token, cookies))
}

func replaceSnapshot(ctx context.Context, path string, original, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".suno-session-refresh-*")
	if err != nil {
		return &Error{Code: "temporary", cause: ErrTemporary}
	}
	name := temporary.Name()
	defer os.Remove(name)
	defer temporary.Close()
	if temporary.Chmod(0600) != nil {
		return &Error{Code: "temporary", cause: ErrTemporary}
	}
	if _, err := temporary.Write(data); err != nil {
		return &Error{Code: "temporary", cause: ErrTemporary}
	}
	if temporary.Sync() != nil || temporary.Close() != nil {
		return &Error{Code: "temporary", cause: ErrTemporary}
	}
	current, err := readSnapshot(path)
	if err != nil || !bytes.Equal(current, original) {
		return &Error{Code: "file_conflict", cause: ErrConflict}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return &Error{Code: "temporary", cause: ErrTemporary}
	}
	return nil
}

func readSnapshot(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, &Error{Code: "refresh_credentials_required", cause: ErrRefreshCredentialsRequired}
	}
	data, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(data) > 65536 {
		return nil, &Error{Code: "refresh_credentials_required", cause: ErrRefreshCredentialsRequired}
	}
	return data, nil
}

func boundedBody(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, &Error{Code: "invalid_response", cause: ErrInvalidResponse}
	}
	return data, nil
}
