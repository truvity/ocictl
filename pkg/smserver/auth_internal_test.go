package smserver

import (
	"context"
	"testing"
	"time"
)

type countingTokens struct{ hosts []string }

func (c *countingTokens) Token(_ context.Context, host string) (Token, error) {
	c.hosts = append(c.hosts, host)
	return Token{Username: "AWS", Password: "pw", Expiry: time.Now().Add(time.Hour)}, nil
}

func TestCredentialsOnlyForConfiguredHosts(t *testing.T) {
	tokens := &countingTokens{}
	cfg := &Config{Auth: Auth{Mode: AuthECR}}
	allowed := map[string]bool{"registry.example": true}

	fn, err := credentialFunc(cfg, allowed, tokens)
	if err != nil {
		t.Fatal(err)
	}

	got, err := fn(context.Background(), "registry.example")
	if err != nil || got.Password != "pw" {
		t.Fatalf("own host: %+v %v", got, err)
	}

	got, err = fn(context.Background(), "token-service.elsewhere.example")
	if err != nil || got.Password != "" || got.Username != "" {
		t.Fatalf("foreign host got a credential: %+v %v", got, err)
	}

	if len(tokens.hosts) != 1 {
		t.Fatalf("token minted for %v", tokens.hosts)
	}
}

func TestECRHostPattern(t *testing.T) {
	account := "1111" + "2222" + "3333"
	cases := map[string][]string{
		account + ".dkr.ecr.eu-west-1.amazonaws.com":      {account, "", "eu-west-1"},
		account + ".dkr.ecr-fips.us-east-1.amazonaws.com": {account, "-fips", "us-east-1"},
		account + ".dkr.ecr.cn-north-1.amazonaws.com.cn":  {account, "", "cn-north-1"},
	}

	for host, want := range cases {
		m := ecrHostPattern.FindStringSubmatch(host)
		if m == nil || m[1] != want[0] || m[2] != want[1] || m[3] != want[2] {
			t.Errorf("%s: %v", host, m)
		}
	}

	ecr := account + ".dkr.ecr.eu-west-1.amazonaws.com"

	for _, host := range []string{"ghcr.io", "evil.example/" + ecr, ecr + ".evil.example"} {
		if ecrHostPattern.MatchString(host) {
			t.Errorf("%s matched", host)
		}
	}
}
