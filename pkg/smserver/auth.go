package smserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// RefreshMargin is how long before expiry a cached registry token is
// replaced.
const RefreshMargin = 5 * time.Minute

type (
	// Token is a registry login.
	Token struct {
		Username string
		Password string
		Expiry   time.Time
	}

	// TokenProvider mints registry logins for a registry host.
	TokenProvider interface {
		Token(ctx context.Context, host string) (Token, error)
	}

	cachingProvider struct {
		inner  TokenProvider
		clock  func() time.Time
		mu     sync.Mutex
		tokens map[string]Token
	}

	awsECRProvider struct {
		mu      sync.Mutex
		clients map[string]*ecr.Client
	}
)

func newCachingProvider(inner TokenProvider, clock func() time.Time) *cachingProvider {
	return &cachingProvider{inner: inner, clock: clock, tokens: map[string]Token{}}
}

// Token returns a cached token until RefreshMargin before it expires.
func (c *cachingProvider) Token(ctx context.Context, host string) (Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if t, ok := c.tokens[host]; ok && c.clock().Before(t.Expiry.Add(-RefreshMargin)) {
		return t, nil
	}

	t, err := c.inner.Token(ctx, host)
	if err != nil {
		return Token{}, err
	}

	c.tokens[host] = t

	return t, nil
}

// NewAWSECRProvider returns a TokenProvider calling ecr:GetAuthorizationToken
// with the default AWS credential chain, in the region parsed from the ECR
// host.
func NewAWSECRProvider() TokenProvider {
	return &awsECRProvider{clients: map[string]*ecr.Client{}}
}

func (p *awsECRProvider) Token(ctx context.Context, host string) (Token, error) {
	m := ecrHostPattern.FindStringSubmatch(host)
	if m == nil {
		return Token{}, fmt.Errorf("%q is not an ECR host", host)
	}

	region := m[3]

	p.mu.Lock()
	client := p.clients[region]

	if client == nil {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
		if err != nil {
			p.mu.Unlock()
			return Token{}, fmt.Errorf("load AWS config: %w", err)
		}

		client = ecr.NewFromConfig(cfg)
		p.clients[region] = client
	}
	p.mu.Unlock()

	out, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return Token{}, fmt.Errorf("ecr:GetAuthorizationToken: %w", err)
	}

	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return Token{}, fmt.Errorf("ecr:GetAuthorizationToken returned no token")
	}

	data := out.AuthorizationData[0]

	raw, err := base64.StdEncoding.DecodeString(*data.AuthorizationToken)
	if err != nil {
		return Token{}, fmt.Errorf("decode ECR token: %w", err)
	}

	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return Token{}, fmt.Errorf("ECR token is not user:password")
	}

	t := Token{Username: user, Password: pass, Expiry: time.Now().Add(time.Hour)}
	if data.ExpiresAt != nil {
		t.Expiry = *data.ExpiresAt
	}

	return t, nil
}

// credentialFunc builds the oras credential callback for the mode. Only
// hosts in allowed (the configured repositories' own hosts) ever get a
// credential; everything else, including a token service on another host,
// is anonymous.
func credentialFunc(
	cfg *Config,
	allowed map[string]bool,
	ecrTokens TokenProvider,
) (func(context.Context, string) (auth.Credential, error), error) {
	guard := func(inner func(context.Context, string) (auth.Credential, error)) func(context.Context, string) (auth.Credential, error) {
		return func(ctx context.Context, host string) (auth.Credential, error) {
			if !allowed[host] {
				return auth.EmptyCredential, nil
			}

			return inner(ctx, host)
		}
	}

	switch cfg.Auth.Mode {
	case AuthAnonymous:
		return func(context.Context, string) (auth.Credential, error) { return auth.EmptyCredential, nil }, nil
	case AuthECR:
		return guard(func(ctx context.Context, host string) (auth.Credential, error) {
			t, err := ecrTokens.Token(ctx, host)
			if err != nil {
				return auth.EmptyCredential, err
			}

			return auth.Credential{Username: t.Username, Password: t.Password}, nil
		}), nil
	case AuthDockerConfig:
		store, err := credentials.NewStore(cfg.Auth.DockerConfig, credentials.StoreOptions{})
		if err != nil {
			return nil, fmt.Errorf("open docker config: %w", err)
		}

		return guard(credentials.Credential(store)), nil
	}

	return nil, fmt.Errorf("auth.mode %q", cfg.Auth.Mode)
}
