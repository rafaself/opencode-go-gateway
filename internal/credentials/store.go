// Package credentials manages the gateway's local API credential.
//
// The package deliberately keeps the credential boundary small: values are
// accepted from standard input, loaded only for the gateway process, and
// never included in status output or errors. Linux installations use the
// Secret Service when the system helper is available; the fallback is a
// private, permission-restricted file for environments without a keyring.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	maxCredentialBytes = 4096
	appDirectory       = "opencode-gateway"
	credentialFilename = "credentials"
)

var (
	// ErrNotFound means that no credential has been configured in the store.
	ErrNotFound = errors.New("credential is not configured")

	errInvalidCredential = errors.New("API key must be a single non-empty line without control characters")
)

// Backend identifies where a credential is stored.
type Backend string

const (
	BackendNone    Backend = "none"
	BackendKeyring Backend = "keyring"
	BackendFile    Backend = "file"
)

// Store reads and writes one gateway credential. The zero value is a disabled
// store, which is useful when callers want environment-only configuration.
type Store struct {
	filePath string
	keyring  keyring
}

// Default returns the user-level credential store. It does not touch the
// filesystem until one of its methods is called.
func Default() Store {
	return Store{
		filePath: defaultCredentialPath(),
		keyring:  newDefaultKeyring(),
	}
}

// NewFileStore creates a deterministic permission-restricted file store. It
// is intended for tests and for callers that explicitly choose file storage.
func NewFileStore(path string) Store {
	return Store{filePath: path}
}

// Load returns the configured credential and its storage backend.
func (s Store) Load() (string, Backend, error) {
	if s.filePath != "" {
		value, err := s.loadFile()
		if err == nil {
			return value, BackendFile, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return "", BackendNone, err
		}
	}
	if s.keyring != nil {
		if value, err := s.keyring.Load(); err == nil {
			return value, BackendKeyring, nil
		}
	}
	return "", BackendNone, ErrNotFound
}

// Save stores value without exposing it in a process argument, output, or
// error. The keyring is preferred; a file fallback is used only when the
// keyring is unavailable or rejects the operation.
func (s Store) Save(value string) (Backend, error) {
	value, err := normalize(value)
	if err != nil {
		return BackendNone, err
	}

	if s.keyring != nil {
		if err := s.keyring.Save(value); err == nil {
			if s.filePath != "" {
				if err := s.removeFile(); err != nil && !errors.Is(err, ErrNotFound) {
					return BackendNone, fmt.Errorf("remove old credential copy: %w", err)
				}
			}
			return BackendKeyring, nil
		}
	}

	if s.filePath == "" {
		return BackendNone, errors.New("no credential store is available")
	}
	if err := s.saveFile(value); err != nil {
		return BackendNone, err
	}
	return BackendFile, nil
}

// Remove deletes the configured credential from every enabled backend. It is
// idempotent so cleanup scripts can safely call it more than once.
func (s Store) Remove() error {
	var firstErr error
	if s.keyring != nil {
		if err := s.keyring.Remove(); err != nil && !errors.Is(err, ErrNotFound) {
			firstErr = err
		}
	}
	if s.filePath != "" {
		if err := s.removeFile(); err != nil && !errors.Is(err, ErrNotFound) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Status reports whether a credential is available without returning its
// value. It is intentionally suitable for human-facing diagnostics.
func (s Store) Status() (Backend, error) {
	_, backend, err := s.Load()
	if errors.Is(err, ErrNotFound) {
		return BackendNone, nil
	}
	if err != nil {
		return BackendNone, err
	}
	return backend, nil
}

func (s Store) loadFile() (string, error) {
	if err := validateCredentialPath(s.filePath); err != nil {
		return "", err
	}
	if err := inspectPrivateDirectory(filepath.Dir(s.filePath)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	info, err := os.Lstat(s.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", errors.New("inspect credential store")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("credential store is not a regular file")
	}
	if err := tightenFilePermissions(s.filePath, info.Mode().Perm()); err != nil {
		return "", err
	}

	file, err := os.Open(s.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", errors.New("open credential store")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	if err != nil {
		return "", errors.New("read credential store")
	}
	if len(data) > maxCredentialBytes {
		return "", errors.New("credential store is too large")
	}
	return normalize(string(data))
}

func (s Store) saveFile(value string) error {
	if err := validateCredentialPath(s.filePath); err != nil {
		return err
	}
	if err := ensurePrivateDirectory(filepath.Dir(s.filePath)); err != nil {
		return err
	}
	if info, err := os.Lstat(s.filePath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("credential store is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect credential store")
	}

	temporary, err := os.CreateTemp(filepath.Dir(s.filePath), ".credentials-*")
	if err != nil {
		return errors.New("create temporary credential store")
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.New("protect temporary credential store")
	}
	if _, err := io.WriteString(temporary, value+"\n"); err != nil {
		return errors.New("write credential store")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("sync credential store")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close credential store")
	}
	if err := os.Rename(temporaryPath, s.filePath); err != nil {
		if runtime.GOOS != "windows" {
			return errors.New("replace credential store")
		}
		if removeErr := removeExistingRegularFile(s.filePath); removeErr != nil && !errors.Is(removeErr, ErrNotFound) {
			return errors.New("replace credential store")
		}
		if err := os.Rename(temporaryPath, s.filePath); err != nil {
			return errors.New("replace credential store")
		}
	}
	removeTemporary = false
	if err := os.Chmod(s.filePath, 0o600); err != nil {
		return errors.New("protect credential store")
	}
	return nil
}

func (s Store) removeFile() error {
	if s.filePath == "" {
		return ErrNotFound
	}
	if err := validateCredentialPath(s.filePath); err != nil {
		return err
	}
	info, err := os.Lstat(s.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return errors.New("inspect credential store")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("credential store is not a regular file")
	}
	if err := os.Remove(s.filePath); err != nil {
		return errors.New("remove credential store")
	}
	return nil
}

func defaultCredentialPath() string {
	configDirectory, err := os.UserConfigDir()
	if err != nil || configDirectory == "" {
		return ""
	}
	return filepath.Join(configDirectory, appDirectory, credentialFilename)
}

func validateCredentialPath(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Base(path) != credentialFilename {
		return errors.New("credential store path is invalid")
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if path == "" {
		return errors.New("credential store directory is unavailable")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return errors.New("create credential store directory")
	}
	return inspectPrivateDirectory(path)
}

func inspectPrivateDirectory(path string) error {
	if path == "" {
		return errors.New("credential store directory is unavailable")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("credential store directory is invalid")
	}
	if err := tightenDirectoryPermissions(path, info.Mode().Perm()); err != nil {
		return err
	}
	return nil
}

func tightenDirectoryPermissions(path string, permissions os.FileMode) error {
	if runtime.GOOS == "windows" || permissions&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return errors.New("protect credential store directory")
	}
	return nil
}

func tightenFilePermissions(path string, permissions os.FileMode) error {
	if runtime.GOOS == "windows" || permissions&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return errors.New("protect credential store")
	}
	return nil
}

func removeExistingRegularFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("credential store is not a regular file")
	}
	if err := os.Remove(path); err != nil {
		return errors.New("remove old credential store")
	}
	return nil
}

func normalize(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errInvalidCredential
	}
	if len([]byte(value)) > maxCredentialBytes {
		return "", errInvalidCredential
	}
	for _, runeValue := range value {
		if runeValue < 0x20 || runeValue == 0x7f {
			return "", errInvalidCredential
		}
	}
	return value, nil
}

type keyring interface {
	Load() (string, error)
	Save(string) error
	Remove() error
}

func newDefaultKeyring() keyring {
	if runtime.GOOS != "linux" || strings.TrimSpace(os.Getenv("DBUS_SESSION_BUS_ADDRESS")) == "" {
		return nil
	}
	return newSecretServiceKeyring()
}

const keyringCommandTimeout = 2 * time.Second

func runKeyringCommand(path string, args []string, input string) ([]byte, error) {
	return runCommand(context.Background(), path, args, input)
}
