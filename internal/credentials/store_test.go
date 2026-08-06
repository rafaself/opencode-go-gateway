package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileStoreRoundTripProtectsCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), appDirectory, credentialFilename)
	store := NewFileStore(path)
	const key = "sk-test-credential-value"

	backend, err := store.Save(key)
	if err != nil {
		t.Fatal(err)
	}
	if backend != BackendFile {
		t.Fatalf("backend = %q, want %q", backend, BackendFile)
	}

	loaded, loadedBackend, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded != key || loadedBackend != BackendFile {
		t.Fatalf("loaded credential = %q/%q, want key/file", loaded, loadedBackend)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := fileInfo.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("credential file permissions = %o, want 600", permissions)
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if permissions := directoryInfo.Mode().Perm(); permissions != 0o700 {
		t.Fatalf("credential directory permissions = %o, want 700", permissions)
	}

	backend, err = store.Status()
	if err != nil || backend != BackendFile {
		t.Fatalf("status = %q/%v, want file/nil", backend, err)
	}
	if err := store.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("load after remove = %v, want ErrNotFound", err)
	}
}

func TestFileStoreStatusDoesNotCreateAnEmptyCredentialDirectory(t *testing.T) {
	root := t.TempDir()
	store := NewFileStore(filepath.Join(root, appDirectory, credentialFilename))
	backend, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if backend != BackendNone {
		t.Fatalf("status backend = %q, want none", backend)
	}
	if _, err := os.Stat(filepath.Join(root, appDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created credential directory; stat error = %v", err)
	}
}

func TestFileStoreRejectsUnsafeCredentialInput(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), appDirectory, credentialFilename))
	for _, value := range []string{"", "   ", "key\nsecond", "key\x00value", strings.Repeat("k", maxCredentialBytes+1)} {
		t.Run("invalid", func(t *testing.T) {
			if _, err := store.Save(value); err == nil {
				t.Fatalf("Save(%q) succeeded", value)
			}
		})
	}
}

func TestFileStoreRejectsSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, appDirectory, credentialFilename)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("do-not-touch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	store := NewFileStore(path)
	if _, err := store.Save("new-key"); err == nil {
		t.Fatal("Save followed credential symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "do-not-touch\n" {
		t.Fatalf("symlink target changed: %q", data)
	}
}

func TestFileStoreTightensExistingPermissions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, appDirectory, credentialFilename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("existing-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(path)
	value, backend, err := store.Load()
	if err != nil || value != "existing-key" || backend != BackendFile {
		t.Fatalf("Load = %q/%q/%v", value, backend, err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("tightened permissions = %o, want 600", fileInfo.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("tightened directory permissions = %o, want 700", directoryInfo.Mode().Perm())
	}
}

func TestStoreFileFallbackWinsOverUnavailableStaleKeyring(t *testing.T) {
	path := filepath.Join(t.TempDir(), appDirectory, credentialFilename)
	keyring := &fakeKeyring{value: "old-key", saveErr: errors.New("keyring unavailable")}
	store := Store{filePath: path, keyring: keyring}
	backend, err := store.Save("new-key")
	if err != nil {
		t.Fatal(err)
	}
	if backend != BackendFile {
		t.Fatalf("backend = %q, want file fallback", backend)
	}
	value, backend, err := store.Load()
	if err != nil || value != "new-key" || backend != BackendFile {
		t.Fatalf("Load = %q/%q/%v, want new-key/file/nil", value, backend, err)
	}
}

type fakeKeyring struct {
	value   string
	saveErr error
}

func (keyring *fakeKeyring) Load() (string, error) {
	if keyring.value == "" {
		return "", ErrNotFound
	}
	return keyring.value, nil
}

func (keyring *fakeKeyring) Save(value string) error {
	if keyring.saveErr != nil {
		return keyring.saveErr
	}
	keyring.value = value
	return nil
}

func (keyring *fakeKeyring) Remove() error {
	keyring.value = ""
	return nil
}
