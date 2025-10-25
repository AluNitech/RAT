package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

const (
	serviceName = "modernrat-client"
	idKey       = "client-id"
	secretKey   = "client-secret"
)

var (
	ErrNotFound           = errors.New("identity not found")
	ErrBackendUnavailable = errors.New("secure keyring backend unavailable")
)

type credentials struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

func Load() (string, string, error) {
	id, err := keyring.Get(serviceName, idKey)
	switch {
	case err == nil:
		secret, secErr := keyring.Get(serviceName, secretKey)
		if secErr != nil {
			if errors.Is(secErr, keyring.ErrNotFound) {
				return "", "", ErrNotFound
			}
			return fallbackLoadWithCause(secErr)
		}
		if id == "" || secret == "" {
			return "", "", ErrNotFound
		}
		return id, secret, nil
	case errors.Is(err, keyring.ErrNotFound):
		return fallbackLoad()
	default:
		return fallbackLoadWithCause(err)
	}
}

func Save(id, secret string) error {
	if id == "" || secret == "" {
		return fmt.Errorf("identity requires both id and secret")
	}
	if err := keyring.Set(serviceName, idKey, id); err != nil {
		if saveErr := fallbackSave(id, secret); saveErr != nil {
			return joinErrors(ErrBackendUnavailable, err, saveErr)
		}
		return joinErrors(ErrBackendUnavailable, err)
	}
	if err := keyring.Set(serviceName, secretKey, secret); err != nil {
		_ = keyring.Delete(serviceName, idKey)
		if saveErr := fallbackSave(id, secret); saveErr != nil {
			return joinErrors(ErrBackendUnavailable, err, saveErr)
		}
		return joinErrors(ErrBackendUnavailable, err)
	}
	// Clean up fallback storage if we were previously using it.
	if err := fallbackClear(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func Clear() error {
	var errs []error
	if err := keyring.Delete(serviceName, idKey); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		errs = append(errs, err)
	}
	if err := keyring.Delete(serviceName, secretKey); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		errs = append(errs, err)
	}
	if err := fallbackClear(); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return joinErrors(errs...)
}

func fallbackLoad() (string, string, error) {
	path, err := fallbackFilePath()
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", ErrNotFound
		}
		return "", "", err
	}
	var creds credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", "", err
	}
	creds.ID = strings.TrimSpace(creds.ID)
	creds.Secret = strings.TrimSpace(creds.Secret)
	if creds.ID == "" || creds.Secret == "" {
		return "", "", ErrNotFound
	}
	return creds.ID, creds.Secret, nil
}

func fallbackLoadWithCause(cause error) (string, string, error) {
	id, secret, err := fallbackLoad()
	if err != nil {
		return "", "", joinErrors(ErrBackendUnavailable, cause, err)
	}
	return id, secret, joinErrors(ErrBackendUnavailable, cause)
}

func fallbackSave(id, secret string) error {
	path, err := fallbackFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	creds := credentials{ID: strings.TrimSpace(id), Secret: strings.TrimSpace(secret)}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fallbackClear() error {
	path, err := fallbackFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

func fallbackFilePath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(configDir) == "" {
		configDir = os.TempDir()
	}
	return filepath.Join(configDir, "modernrat", "credentials.json"), nil
}

func joinErrors(errs ...error) error {
	filtered := make([]error, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return multiError{errs: filtered}
	}
}

type multiError struct {
	errs []error
}

func (m multiError) Error() string {
	parts := make([]string, 0, len(m.errs))
	for _, err := range m.errs {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "; ")
}

func (m multiError) Unwrap() []error {
	return m.errs
}
