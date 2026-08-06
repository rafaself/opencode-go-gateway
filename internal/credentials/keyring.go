package credentials

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
)

const (
	secretServiceName    = "opencode-go-gateway"
	secretServiceAccount = "default"
)

type secretServiceKeyring struct {
	command string
}

func newSecretServiceKeyring() keyring {
	for _, candidate := range []string{"/usr/bin/secret-tool", "/usr/local/bin/secret-tool"} {
		if info, err := exec.LookPath(candidate); err == nil && info == candidate {
			return secretServiceKeyring{command: candidate}
		}
	}
	return nil
}

func (k secretServiceKeyring) Load() (string, error) {
	output, err := runKeyringCommand(k.command, []string{
		"lookup",
		"service", secretServiceName,
		"account", secretServiceAccount,
	}, "")
	if err != nil {
		return "", ErrNotFound
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", ErrNotFound
	}
	return normalize(value)
}

func (k secretServiceKeyring) Save(value string) error {
	_, err := runKeyringCommand(k.command, []string{
		"store",
		"--label", "OpenCode Go Gateway API key",
		"service", secretServiceName,
		"account", secretServiceAccount,
	}, value+"\n")
	return err
}

func (k secretServiceKeyring) Remove() error {
	if _, err := k.Load(); err != nil {
		return err
	}
	_, err := runKeyringCommand(k.command, []string{
		"clear",
		"service", secretServiceName,
		"account", secretServiceAccount,
	}, "")
	if err != nil {
		return err
	}
	return nil
}

func runCommand(parent context.Context, path string, args []string, input string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("credential helper is unavailable")
	}
	ctx, cancel := context.WithTimeout(parent, keyringCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, args...)
	command.Stdin = strings.NewReader(input)
	command.Stderr = io.Discard
	var output bytes.Buffer
	command.Stdout = &output
	if err := command.Run(); err != nil {
		return nil, errors.New("credential helper failed")
	}
	return output.Bytes(), nil
}
