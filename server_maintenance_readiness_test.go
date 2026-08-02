package main

import (
	"context"
	"errors"
	"testing"

	serverpkg "debian-updater/internal/servers"
)

type readinessCredentialStore struct {
	value string
	err   error
}

func (s *readinessCredentialStore) Read(context.Context) (string, error) {
	return s.value, s.err
}

func (s *readinessCredentialStore) Write(_ context.Context, value string) error {
	s.value = value
	return nil
}

func (s *readinessCredentialStore) Delete(context.Context) error {
	s.value = ""
	return nil
}

func TestGlobalSSHCredentialReadinessUsesCachedResolutionOnReadFailure(t *testing.T) {
	store := &readinessCredentialStore{}
	credential := serverpkg.NewGlobalSSHCredential(serverpkg.GlobalSSHCredentialDeps{
		Store:    store,
		Encrypt:  func(value string) (string, error) { return value, nil },
		Decrypt:  func(value string) (string, error) { return value, nil },
		Validate: func(string) error { return nil },
	})
	if result := credential.Replace(context.Background(), "last-known-good"); !result.Succeeded() {
		t.Fatalf("Replace() = %+v, want success", result)
	}
	store.err = errors.New("database is locked")

	configured, known := globalSSHCredentialReadiness(credential)
	if !configured || !known {
		t.Fatalf("globalSSHCredentialReadiness() = configured=%t known=%t, want true, true", configured, known)
	}
}

func TestGlobalSSHCredentialReadinessKeepsUnknownWithoutCachedResolution(t *testing.T) {
	credential := serverpkg.NewGlobalSSHCredential(serverpkg.GlobalSSHCredentialDeps{
		Store: &readinessCredentialStore{err: errors.New("database is locked")},
	})

	configured, known := globalSSHCredentialReadiness(credential)
	if configured || known {
		t.Fatalf("globalSSHCredentialReadiness() = configured=%t known=%t, want false, false", configured, known)
	}
}
