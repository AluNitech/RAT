package identity

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

const (
	serviceName = "modernrat-client"
	idKey       = "client-id"
	secretKey   = "client-secret"
)

var ErrNotFound = errors.New("identity not found")

func Load() (string, string, error) {
	id, err := keyring.Get(serviceName, idKey)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", "", ErrNotFound
		}
		return "", "", err
	}

	secret, err := keyring.Get(serviceName, secretKey)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", "", ErrNotFound
		}
		return "", "", err
	}

	if id == "" || secret == "" {
		return "", "", ErrNotFound
	}

	return id, secret, nil
}

func Save(id, secret string) error {
	if id == "" || secret == "" {
		return fmt.Errorf("identity requires both id and secret")
	}
	if err := keyring.Set(serviceName, idKey, id); err != nil {
		return err
	}
	if err := keyring.Set(serviceName, secretKey, secret); err != nil {
		_ = keyring.Delete(serviceName, idKey)
		return err
	}
	return nil
}

func Clear() error {
	var multiErr error
	if err := keyring.Delete(serviceName, idKey); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		multiErr = err
	}
	if err := keyring.Delete(serviceName, secretKey); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		if multiErr == nil {
			multiErr = err
		}
	}
	return multiErr
}
