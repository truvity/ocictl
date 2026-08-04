package fleettest

import (
	"context"
	"strings"
	"testing"

	"github.com/truvity/ocictl/pkg/kubewho"
)

// clearFleetEnv isolates a test from the ambient FLEET_*/CI environment
// (and, via t.Setenv's cleanup, from what Export writes back).
func clearFleetEnv(t *testing.T) {
	t.Helper()

	for _, k := range []string{EnvNamespace, EnvRelease, EnvKubecontext, EnvCIRunNumber, EnvCIRunAttempt} {
		t.Setenv(k, "")
	}
}

// fakeWhoAmI is the injection seam that keeps resolution unit-testable:
// no kubectl, no cluster.
func fakeWhoAmI(groups ...string) func(context.Context, kubewho.Options) (kubewho.User, error) {
	return func(context.Context, kubewho.Options) (kubewho.User, error) {
		return kubewho.User{Username: "o.tsarev@truvity.com", Groups: groups}, nil
	}
}

func TestResolveLadders(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		opts    Options
		want    Tenant
		wantErr string
	}{
		{
			name: "options win over everything",
			env:  map[string]string{EnvNamespace: "env-ns", EnvRelease: "env-rel", EnvCIRunNumber: "7"},
			opts: Options{Namespace: "opt-ns", Release: "opt-rel", App: "app", WhoAmI: fakeWhoAmI("emp:otsar")},
			want: Tenant{Namespace: "opt-ns", Release: "opt-rel"},
		},
		{
			name: "env wins over identity and CI",
			env:  map[string]string{EnvNamespace: "ci-truvity-bar", EnvRelease: "r12-a2", EnvCIRunNumber: "7"},
			opts: Options{App: "url-shortener", WhoAmI: fakeWhoAmI("emp:otsar")},
			want: Tenant{Namespace: "ci-truvity-bar", Release: "r12-a2"},
		},
		{
			name: "employee standing install: identity namespace, app release",
			opts: Options{App: "url-shortener", WhoAmI: fakeWhoAmI("emp:otsar", "system:authenticated")},
			want: Tenant{Namespace: "emp-otsar", Release: "url-shortener"},
		},
		{
			name: "personal-namespace template and group prefix are parameters",
			opts: Options{
				App:               "url-shortener",
				GroupPrefix:       "dev:",
				PersonalNamespace: "sandbox-{slug}-ns",
				WhoAmI:            fakeWhoAmI("dev:kim", "emp:ignored"),
			},
			want: Tenant{Namespace: "sandbox-kim-ns", Release: "url-shortener"},
		},
		{
			name: "CI release beats the app name",
			env:  map[string]string{EnvCIRunNumber: "412", EnvCIRunAttempt: "3"},
			opts: Options{Namespace: "ci-truvity-bar", App: "url-shortener"},
			want: Tenant{Namespace: "ci-truvity-bar", Release: "r412-a3"},
		},
		{
			name: "CI attempt defaults to 1",
			env:  map[string]string{EnvCIRunNumber: "9"},
			opts: Options{Namespace: "ci-truvity-bar"},
			want: Tenant{Namespace: "ci-truvity-bar", Release: "r9-a1"},
		},
		{
			name: "an empty run number is not CI",
			env:  map[string]string{EnvCIRunNumber: "  ", EnvCIRunAttempt: "2"},
			opts: Options{Namespace: "emp-otsar", App: "url-shortener"},
			want: Tenant{Namespace: "emp-otsar", Release: "url-shortener"},
		},
		{
			name: "ephemeral falls back to one release per namespace",
			opts: Options{Ephemeral: true, Namespace: "it-abc12345"},
			want: Tenant{Namespace: "it-abc12345", Release: "it-abc12345"},
		},
		{
			name:    "standing with no release is an error, not a guess",
			opts:    Options{Namespace: "emp-otsar"},
			wantErr: "no release",
		},
		{
			name:    "zero identity groups is an error",
			opts:    Options{App: "url-shortener", WhoAmI: fakeWhoAmI("system:authenticated")},
			wantErr: `no "emp:" group`,
		},
		{
			name:    "multiple identity groups is an error",
			opts:    Options{App: "url-shortener", WhoAmI: fakeWhoAmI("emp:a", "emp:b")},
			wantErr: "refusing to guess",
		},
		{
			name:    "a template without {slug} is an error",
			opts:    Options{App: "app", PersonalNamespace: "static", WhoAmI: fakeWhoAmI("emp:otsar")},
			wantErr: "must contain {slug}",
		},
		{
			name:    "release over the helm bound is rejected",
			opts:    Options{Namespace: "emp-otsar", App: strings.Repeat("a", MaxReleaseLen+1)},
			wantErr: "over the 53-character bound",
		},
		{
			name: "release exactly at the helm bound is accepted",
			opts: Options{Namespace: "emp-otsar", App: strings.Repeat("a", MaxReleaseLen)},
			want: Tenant{Namespace: "emp-otsar", Release: strings.Repeat("a", MaxReleaseLen)},
		},
		{
			name:    "namespace over the label bound is rejected",
			opts:    Options{Namespace: strings.Repeat("n", MaxNamespaceLen+1), App: "app"},
			wantErr: "over the 63-character bound",
		},
		{
			name:    "a namespace that is not an RFC1123 label is rejected",
			opts:    Options{Namespace: "Emp_Otsar", App: "app"},
			wantErr: "not a valid RFC1123 label",
		},
		{
			name:    "a release that is not an RFC1123 label is rejected",
			opts:    Options{Namespace: "emp-otsar", App: "URL_Shortener"},
			wantErr: "not a valid RFC1123 label",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearFleetEnv(t)

			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			got, err := Resolve(context.Background(), tc.opts)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v (tenant %v)", tc.wantErr, err, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolve: %v", err)
			}

			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The ephemeral namespace still derives from the git sha — the pilot's
// model stays reachable.
func TestResolveEphemeralDerivesFromGitSHA(t *testing.T) {
	clearFleetEnv(t)

	sha, err := gitShortSHA(context.Background())
	if err != nil {
		t.Skipf("no git work tree: %v", err)
	}

	got, err := Resolve(context.Background(), Options{Ephemeral: true, NamespacePrefix: "it-"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got.Namespace != "it-"+sha {
		t.Errorf("namespace %q, want it-%s", got.Namespace, sha)
	}
}

// ResolveNamespace is the same ladder without the release half — whoami
// prints a namespace even when no release can be determined.
func TestResolveNamespaceAlone(t *testing.T) {
	clearFleetEnv(t)

	got, err := ResolveNamespace(context.Background(), Options{WhoAmI: fakeWhoAmI("emp:otsar")})
	if err != nil || got != "emp-otsar" {
		t.Errorf("got %q err %v", got, err)
	}

	if _, err := ResolveNamespace(context.Background(), Options{Namespace: "Bad_NS"}); err == nil {
		t.Error("bounds are asserted here too")
	}
}

// Resolve must be pure: it may not write the env its callers export.
func TestResolveWritesNoEnv(t *testing.T) {
	clearFleetEnv(t)

	if _, err := Resolve(context.Background(), Options{Namespace: "emp-otsar", App: "url-shortener"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if Namespace() != "" || Release() != "" {
		t.Errorf("Resolve exported env: namespace=%q release=%q", Namespace(), Release())
	}
}

func TestTenantEnvAndExport(t *testing.T) {
	clearFleetEnv(t)

	tenant := Tenant{Namespace: "emp-otsar", Release: "url-shortener"}

	want := []string{
		EnvNamespace + "=emp-otsar",
		EnvRelease + "=url-shortener",
		EnvKubecontext + "=devel@oidc",
	}

	got := tenant.Env("devel@oidc")
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("env %v, want %v", got, want)
	}

	if kv := tenant.Env(""); len(kv) != 2 {
		t.Errorf("no kubecontext must export two pairs, got %v", kv)
	}

	if err := tenant.Export("devel@oidc"); err != nil {
		t.Fatalf("export: %v", err)
	}

	if Namespace() != "emp-otsar" || Release() != "url-shortener" {
		t.Errorf("accessors after export: namespace=%q release=%q", Namespace(), Release())
	}

	if tenant.String() != "emp-otsar/url-shortener" {
		t.Errorf("String() = %q", tenant.String())
	}
}
