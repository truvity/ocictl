package ocipush

import (
	"context"

	"oras.land/oras-go/v2/registry/remote/auth"
)

// WriteCredentials selects the credential source for pushing to repoRef; it is
// the one Push uses (GITHUB_TOKEN for ghcr.io, the Docker credential store
// otherwise).
func WriteCredentials(repoRef, awsProfile string) func(ctx context.Context, hostport string) (auth.Credential, error) {
	return resolveCredentials(repoRef, awsProfile)
}
