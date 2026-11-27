// Package registry opens an OCI registry repository with the credentials of
// the Docker config, the way every vm-manager command that talks to a
// registry does.
package registry

import (
	"fmt"
	"os"
	"path/filepath"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// Repository opens the repository of reference
// (<registry>/<repository>[:<tag>|@<digest>]). Credentials come from the
// Docker config ($DOCKER_CONFIG/config.json or ~/.docker/config.json) when
// one exists; without one the access is anonymous. plainHTTP talks to the
// registry over HTTP, for a lab registry.
func Repository(reference string, plainHTTP bool) (*remote.Repository, error) {
	repo, err := remote.NewRepository(reference)
	if err != nil {
		return nil, fmt.Errorf("reference %q: %w", reference, err)
	}
	repo.PlainHTTP = plainHTTP
	client := &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}
	if store := dockerCredentials(); store != nil {
		client.Credential = credentials.Credential(store)
	}
	repo.Client = client
	return repo, nil
}

// dockerCredentials returns the Docker config credential store when a config
// file exists, else nil for anonymous access — the store is never created, so
// a read-only root file system is fine.
func dockerCredentials() credentials.Store {
	configDir := os.Getenv("DOCKER_CONFIG")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		configDir = filepath.Join(home, ".docker")
	}
	path := filepath.Join(configDir, "config.json")
	if _, err := os.Stat(path); err != nil { //nolint:gosec // G703: the Docker config the user points DOCKER_CONFIG at
		return nil
	}
	store, err := credentials.NewStore(path, credentials.StoreOptions{})
	if err != nil {
		return nil
	}
	return store
}
