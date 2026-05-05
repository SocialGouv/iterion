//go:build desktop

package main

import (
	"os"
	"testing"
)

type fakeSecretStore struct {
	values map[string]string
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{values: make(map[string]string)}
}

func (s *fakeSecretStore) Get(key string) (string, error) {
	return s.values[key], nil
}

func (s *fakeSecretStore) Set(key, value string) error {
	s.values[key] = value
	return nil
}

func (s *fakeSecretStore) Delete(key string) error {
	delete(s.values, key)
	return nil
}

func statusFor(t *testing.T, statuses []SecretStatus, key string) SecretStatus {
	t.Helper()
	for _, st := range statuses {
		if st.Key == key {
			return st
		}
	}
	t.Fatalf("status for %s not found", key)
	return SecretStatus{}
}

func TestKeychainInjectedSecretIsNotShadowed(t *testing.T) {
	key := "ANTHROPIC_API_KEY"
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	store := newFakeSecretStore()
	store.values[key] = "stored-value"
	app := &App{keychain: store}

	app.applyKeychainToEnv()

	if got := os.Getenv(key); got != "stored-value" {
		t.Fatalf("expected keychain value injected into env, got %q", got)
	}
	st := statusFor(t, app.GetSecretStatuses(), key)
	if !st.Stored {
		t.Fatalf("expected stored=true")
	}
	if st.Shadowed {
		t.Fatalf("expected keychain-injected env not to be shadowed")
	}
}

func TestSetSecretUpdatesInjectedEnvImmediately(t *testing.T) {
	key := "OPENAI_API_KEY"
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	store := newFakeSecretStore()
	store.values[key] = "old-value"
	app := &App{keychain: store}
	app.applyKeychainToEnv()

	if err := app.SetSecret(key, "new-value"); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(key); got != "new-value" {
		t.Fatalf("expected edited key to update process env immediately, got %q", got)
	}
}

func TestDeleteSecretRemovesInjectedEnv(t *testing.T) {
	key := "OPENROUTER_API_KEY"
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	store := newFakeSecretStore()
	store.values[key] = "stored-value"
	app := &App{keychain: store}
	app.applyKeychainToEnv()

	if err := app.DeleteSecret(key); err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv(key); ok {
		t.Fatalf("expected delete to remove Iterion-injected env")
	}
}

func TestShellEnvRemainsShadowedAndIsNeverOverwritten(t *testing.T) {
	key := "GROQ_API_KEY"
	t.Setenv(key, "shell-value")
	store := newFakeSecretStore()
	store.values[key] = "stored-value"
	app := &App{keychain: store}
	app.applyKeychainToEnv()

	if got := os.Getenv(key); got != "shell-value" {
		t.Fatalf("expected shell env to win over keychain injection, got %q", got)
	}
	st := statusFor(t, app.GetSecretStatuses(), key)
	if !st.Stored || !st.Shadowed {
		t.Fatalf("expected stored=true shadowed=true, got %+v", st)
	}
	if err := app.SetSecret(key, "edited-value"); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(key); got != "shell-value" {
		t.Fatalf("expected shell env not overwritten by edit, got %q", got)
	}
	if err := app.DeleteSecret(key); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(key); got != "shell-value" {
		t.Fatalf("expected shell env not unset by delete, got %q", got)
	}
}
