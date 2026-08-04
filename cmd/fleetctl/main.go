// Command fleetctl provides identity- and namespace-aware Kubernetes
// primitives, designed to be called from build-system tasks (see
// docs/rfc-fleet.md).
//
// Usage:
//
//	fleetctl deploy -c <kubecontext> --chart <ref|path> [--build] [release]
//	fleetctl test  [-c <kubecontext>] [--ephemeral] [--keep] [--build] -- <command...>
//	fleetctl whoami [-c <kubecontext>]
//
// Tenant resolution (one path, shared by all three — see
// pkg/fleettest.Resolve): namespace is --namespace → FLEET_NAMESPACE →
// derived from the cluster's view of the caller (kubectl auth whoami →
// prefixed identity group → personal-namespace template); release is
// --release → FLEET_RELEASE → the CI run → --app. kubectl is the
// abstraction: no kubeconfig parsing, no client-go.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"
	"go.yaml.in/yaml/v3"

	"github.com/truvity/ocictl/pkg/fleetcfg"
	"github.com/truvity/ocictl/pkg/fleettest"
	"github.com/truvity/ocictl/pkg/kubewho"
)

var Version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	err := app().Run(ctx, os.Args)

	cancel()

	if err != nil {
		fmt.Fprintf(os.Stderr, "fleetctl: %v\n", err)
		os.Exit(1)
	}
}

// commonFlags are shared by every subcommand; fleet.yaml supplies the
// committed defaults and flags/env always win (flags → FLEET_* env →
// fleet.yaml — see the RFC's precedence rule).
func commonFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "kubecontext",
			Aliases: []string{"c"},
			Usage:   "kubeconfig context name (kubeconfig is the cluster registry)",
			Sources: cli.EnvVars("FLEET_KUBECONTEXT"),
		},
		&cli.StringFlag{
			Name:    "kubeconfig",
			Usage:   "override the ambient KUBECONFIG",
			Sources: cli.EnvVars("FLEET_KUBECONFIG"),
		},
		&cli.StringFlag{
			Name:    "namespace",
			Aliases: []string{"n"},
			Usage:   "target namespace (skips identity resolution)",
			Sources: cli.EnvVars("FLEET_NAMESPACE"),
		},
		&cli.StringFlag{
			Name:    "release",
			Aliases: []string{"r"},
			Usage:   "release name — the other half of the tenant identity",
			Sources: cli.EnvVars("FLEET_RELEASE"),
		},
		&cli.StringFlag{
			Name:    "app",
			Usage:   "application name: the release of a standing employee install",
			Sources: cli.EnvVars("FLEET_APP"),
		},
		&cli.StringFlag{
			Name:    "project",
			Usage:   "project name for per-project fleet.yaml hooks (default: --app)",
			Sources: cli.EnvVars("FLEET_PROJECT"),
		},
		&cli.StringFlag{
			Name:    "config",
			Usage:   "fleet.yaml path (optional file)",
			Value:   fleetcfg.DefaultPath,
			Sources: cli.EnvVars("FLEET_CONFIG"),
		},
		&cli.StringFlag{
			Name:    "personal-namespace",
			Usage:   "personal namespace template, e.g. emp-{slug}",
			Sources: cli.EnvVars("FLEET_PERSONAL_NAMESPACE"),
		},
		&cli.StringFlag{
			Name:    "group-prefix",
			Usage:   "identity group prefix, e.g. emp:",
			Sources: cli.EnvVars("FLEET_GROUP_PREFIX"),
		},
	}
}

func app() *cli.Command {
	return &cli.Command{
		Name:    "fleetctl",
		Usage:   "identity- and namespace-aware Kubernetes primitives",
		Version: Version,
		Commands: []*cli.Command{
			deployCommand(),
			testCommand(),
			whoamiCommand(),
		},
	}
}

func deployCommand() *cli.Command {
	return &cli.Command{
		Name: "deploy",
		Usage: "helm upgrade --install into the resolved tenant, " +
			"with the cluster's fleet values merged as .Values.fleet",
		ArgsUsage: "[release]",
		Flags: append(commonFlags(),
			&cli.StringFlag{Name: "chart", Usage: "chart: local path or OCI ref", Required: true},
			&cli.BoolFlag{Name: "build", Usage: "run the fleet.yaml build hook first"},
			&cli.StringSliceFlag{Name: "set", Usage: "passed through to helm --set"},
			&cli.StringSliceFlag{Name: "values", Aliases: []string{"f"}, Usage: "passed through to helm --values"},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := fleetcfg.Load(cmd.String("config"))
			if err != nil {
				return err
			}

			opts := tenantOptions(cmd, cfg)
			if release := cmd.Args().First(); release != "" {
				opts.Release = release // the positional argument wins
			}

			// The same resolution the test harness uses — deploy and test
			// cannot disagree about which tenant they mean.
			tenant, err := fleettest.Resolve(ctx, opts)
			if err != nil {
				return err
			}

			if cmd.Bool("build") {
				if err := runBuildHook(ctx, cfg, projectName(cmd), tenant, opts.Kubecontext); err != nil {
					return err
				}
			}

			args := []string{
				"upgrade", "--install", tenant.Release, cmd.String("chart"),
				"--namespace", tenant.Namespace,
			}
			if opts.Kubecontext != "" {
				args = append(args, "--kube-context", opts.Kubecontext)
			}

			// The cluster's opaque values ride as .Values.fleet — written
			// to a temp file so helm's own precedence rules stay intact
			// (later -f/--set flags win over it).
			if cluster, ok := cfg.Clusters[opts.Kubecontext]; ok && len(cluster.Values) > 0 {
				tmp, tmpErr := writeFleetValues(cluster.Values)
				if tmpErr != nil {
					return tmpErr
				}
				defer func() { _ = os.Remove(tmp) }()

				args = append(args, "--values", tmp)
			}

			for _, f := range cmd.StringSlice("values") {
				args = append(args, "--values", f)
			}

			for _, s := range cmd.StringSlice("set") {
				args = append(args, "--set", s)
			}

			return runPassthrough(ctx, cmd.String("kubeconfig"), tenant, opts.Kubecontext, "helm", args...)
		},
	}
}

func testCommand() *cli.Command {
	return &cli.Command{
		Name: "test",
		Usage: "run a command inside the resolved tenant " +
			"(standing by default; --ephemeral creates and deletes a namespace)",
		ArgsUsage: "-- <command...>",
		Flags: append(commonFlags(),
			&cli.BoolFlag{
				Name:  "ephemeral",
				Usage: "create {prefix}{git-sha}, run, tear down (default: use the standing tenant, create nothing)",
			},
			&cli.StringFlag{Name: "prefix", Usage: "ephemeral namespace prefix", Value: "it-"},
			&cli.BoolFlag{Name: "keep", Usage: "keep the ephemeral namespace on exit (debugging)"},
			&cli.BoolFlag{Name: "build", Usage: "run the fleet.yaml build hook first"},
		),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := fleetcfg.Load(cmd.String("config"))
			if err != nil {
				return err
			}

			argv := cmd.Args().Slice()
			if len(argv) == 0 {
				return fmt.Errorf("no command: fleetctl test [flags] -- <command...>")
			}

			opts := tenantOptions(cmd, cfg)
			opts.Ephemeral = cmd.Bool("ephemeral")
			opts.NamespacePrefix = cmd.String("prefix")
			opts.Keep = cmd.Bool("keep")

			runner, err := fleettest.New(ctx, opts)
			if err != nil {
				return err
			}

			// After New, so the hook sees the resolved tenant.
			if cmd.Bool("build") {
				if err := runBuildHook(ctx, cfg, projectName(cmd), runner.Tenant(), opts.Kubecontext); err != nil {
					_ = runner.Close()

					return err
				}
			}

			code, execErr := runner.Exec(ctx, argv)

			if closeErr := runner.Close(); closeErr != nil {
				fmt.Fprintf(os.Stderr, "fleetctl: teardown: %v\n", closeErr)

				if code == 0 {
					code = 1
				}
			}

			if execErr != nil {
				return execErr
			}

			if code != 0 {
				// Propagate the child's exit code without an extra error line.
				os.Exit(code)
			}

			return nil
		},
	}
}

func whoamiCommand() *cli.Command {
	return &cli.Command{
		Name:  "whoami",
		Usage: "the cluster's view of the caller + the resolved tenant",
		Flags: commonFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := fleetcfg.Load(cmd.String("config"))
			if err != nil {
				return err
			}

			opts := tenantOptions(cmd, cfg)

			user, err := kubewho.WhoAmI(ctx, kubewho.Options{
				Kubecontext: opts.Kubecontext,
				Kubeconfig:  opts.Kubeconfig,
			})
			if err != nil {
				return err
			}

			fmt.Printf("username:  %s\n", user.Username)

			for _, g := range user.Groups {
				fmt.Printf("group:     %s\n", g)
			}

			prefix := stringOr(opts.GroupPrefix, fleettest.DefaultGroupPrefix)

			if slug, slugErr := kubewho.GroupPrefixed(user, prefix); slugErr == nil {
				fmt.Printf("identity:  %s\n", slug)
			} else {
				fmt.Printf("identity:  (none: %v)\n", slugErr)
			}

			// Reuse the identity already fetched: one kubectl call, and the
			// tenant printed here is exactly what deploy/test would use.
			opts.WhoAmI = func(context.Context, kubewho.Options) (kubewho.User, error) { return user, nil }

			// The two halves print independently: a missing release must
			// not hide the namespace, which is what whoami is usually for.
			namespace, nsErr := fleettest.ResolveNamespace(ctx, opts)
			if nsErr != nil {
				fmt.Printf("namespace: (unresolved: %v)\n", nsErr)
			} else {
				fmt.Printf("namespace: %s\n", namespace)
				opts.Namespace = namespace
			}

			tenant, err := fleettest.Resolve(ctx, opts)
			if err != nil {
				fmt.Printf("release:   (unresolved: %v)\n", err)

				return nil
			}

			fmt.Printf("release:   %s\n", tenant.Release)

			return nil
		},
	}
}

// tenantOptions builds the resolution input shared by every command:
// flags (which already carry their FLEET_* env sources) over fleet.yaml.
func tenantOptions(cmd *cli.Command, cfg fleetcfg.Config) fleettest.Options {
	return fleettest.Options{
		Kubecontext:       cmd.String("kubecontext"),
		Kubeconfig:        cmd.String("kubeconfig"),
		Namespace:         cmd.String("namespace"),
		Release:           cmd.String("release"),
		App:               cmd.String("app"),
		PersonalNamespace: stringOr(cmd.String("personal-namespace"), cfg.PersonalNamespace),
		GroupPrefix:       stringOr(cmd.String("group-prefix"), cfg.GroupPrefix),
	}
}

// projectName keys the per-project hooks; the app is the project unless
// the caller says otherwise.
func projectName(cmd *cli.Command) string {
	return stringOr(cmd.String("project"), cmd.String("app"))
}

// runBuildHook executes the repo-owned opaque build argv — the project's
// own hook when fleet.yaml defines one, else the global hook — with the
// resolved FLEET_* env exported.
func runBuildHook(
	ctx context.Context,
	cfg fleetcfg.Config,
	project string,
	tenant fleettest.Tenant,
	kubecontext string,
) error {
	hook := cfg.BuildHook(project)
	if len(hook) == 0 {
		return fmt.Errorf("--build: no hooks.build in fleet.yaml (global or projects.%s.hooks.build)",
			stringOr(project, "<project>"))
	}

	return runPassthrough(ctx, "", tenant, kubecontext, hook[0], hook[1:]...)
}

// runPassthrough runs a subprocess with stdio attached and the resolved
// FLEET_* env re-exported (the RFC's bidirectional-variable rule).
func runPassthrough(
	ctx context.Context,
	kubeconfig string,
	tenant fleettest.Tenant,
	kubecontext, name string,
	args ...string,
) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), tenant.Env(kubecontext)...)

	if kubeconfig != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+kubeconfig)
	}

	return cmd.Run()
}

// writeFleetValues wraps the opaque cluster values under the `fleet:` key
// in a temp file for helm --values.
func writeFleetValues(values map[string]any) (string, error) {
	data, err := yaml.Marshal(map[string]any{"fleet": values})
	if err != nil {
		return "", fmt.Errorf("marshal fleet values: %w", err)
	}

	tmp, err := os.CreateTemp("", "fleet-values-*.yaml")
	if err != nil {
		return "", err
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return "", err
	}

	return tmp.Name(), tmp.Close()
}

func stringOr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}
