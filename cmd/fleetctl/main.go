// Command fleetctl provides identity- and namespace-aware Kubernetes
// primitives, designed to be called from build-system tasks (see
// docs/rfc-fleet.md).
//
// Usage:
//
//	fleetctl deploy -c <kubecontext> --chart <ref|path> [--build] [release]
//	fleetctl test  [-c <kubecontext>] [--prefix it-] [--keep] [--build] -- <command...>
//	fleetctl whoami [-c <kubecontext>]
//
// Namespace resolution: --namespace → FLEET_NAMESPACE → derived from the
// cluster's view of the caller (kubectl auth whoami → prefixed identity
// group → personal-namespace template). kubectl is the abstraction:
// no kubeconfig parsing, no client-go.
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
		Usage: "helm upgrade --install into the resolved namespace, " +
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

			namespace, err := resolveNamespace(ctx, cmd, cfg)
			if err != nil {
				return err
			}

			if cmd.Bool("build") {
				if err := runBuildHook(ctx, cfg, namespace, cmd.String("kubecontext")); err != nil {
					return err
				}
			}

			release := cmd.Args().First()
			if release == "" {
				release = namespace // sane dev-loop default: one release per namespace
			}

			args := []string{"upgrade", "--install", release, cmd.String("chart"), "--namespace", namespace}
			if kctx := cmd.String("kubecontext"); kctx != "" {
				args = append(args, "--kube-context", kctx)
			}

			// The cluster's opaque values ride as .Values.fleet — written
			// to a temp file so helm's own precedence rules stay intact
			// (later -f/--set flags win over it).
			if cluster, ok := cfg.Clusters[cmd.String("kubecontext")]; ok && len(cluster.Values) > 0 {
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

			return runPassthrough(ctx, cmd.String("kubeconfig"), namespace, cmd.String("kubecontext"), "helm", args...)
		},
	}
}

func testCommand() *cli.Command {
	return &cli.Command{
		Name:      "test",
		Usage:     "run a command inside an ephemeral namespace",
		ArgsUsage: "-- <command...>",
		Flags: append(commonFlags(),
			&cli.StringFlag{Name: "prefix", Usage: "ephemeral namespace prefix", Value: "it-"},
			&cli.BoolFlag{Name: "keep", Usage: "keep the namespace on exit (debugging)"},
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

			if cmd.Bool("build") {
				if err := runBuildHook(ctx, cfg, "", cmd.String("kubecontext")); err != nil {
					return err
				}
			}

			runner, err := fleettest.New(ctx, fleettest.Options{
				Kubecontext:     cmd.String("kubecontext"),
				Kubeconfig:      cmd.String("kubeconfig"),
				NamespacePrefix: cmd.String("prefix"),
				Namespace:       cmd.String("namespace"), // FLEET_NAMESPACE override
				Keep:            cmd.Bool("keep"),
			})
			if err != nil {
				return err
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
		Usage: "the cluster's view of the caller + the resolved namespace",
		Flags: commonFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := fleetcfg.Load(cmd.String("config"))
			if err != nil {
				return err
			}

			user, err := kubewho.WhoAmI(ctx, kubewho.Options{
				Kubecontext: cmd.String("kubecontext"),
				Kubeconfig:  cmd.String("kubeconfig"),
			})
			if err != nil {
				return err
			}

			fmt.Printf("username:  %s\n", user.Username)

			for _, g := range user.Groups {
				fmt.Printf("group:     %s\n", g)
			}

			prefix := stringOr(cmd.String("group-prefix"), cfg.GroupPrefix, "emp:")

			slug, slugErr := kubewho.GroupPrefixed(user, prefix)
			if slugErr != nil {
				fmt.Printf("identity:  (none: %v)\n", slugErr)

				return nil
			}

			fmt.Printf("identity:  %s\n", slug)

			if template := stringOr(cmd.String("personal-namespace"), cfg.PersonalNamespace, ""); template != "" {
				if ns, nsErr := fleetcfg.RenderNamespace(template, slug); nsErr == nil {
					fmt.Printf("namespace: %s\n", ns)
				}
			}

			return nil
		},
	}
}

// resolveNamespace implements the RFC ladder: --namespace/FLEET_NAMESPACE
// (the flag carries the env via Sources) → derived personal namespace.
// Lazy by construction: only called by commands that need a namespace.
func resolveNamespace(ctx context.Context, cmd *cli.Command, cfg fleetcfg.Config) (string, error) {
	if ns := cmd.String("namespace"); ns != "" {
		return ns, nil
	}

	user, err := kubewho.WhoAmI(ctx, kubewho.Options{
		Kubecontext: cmd.String("kubecontext"),
		Kubeconfig:  cmd.String("kubeconfig"),
	})
	if err != nil {
		return "", err
	}

	prefix := stringOr(cmd.String("group-prefix"), cfg.GroupPrefix, "emp:")

	slug, err := kubewho.GroupPrefixed(user, prefix)
	if err != nil {
		return "", err
	}

	template := stringOr(cmd.String("personal-namespace"), cfg.PersonalNamespace, "")

	return fleetcfg.RenderNamespace(template, slug)
}

// runBuildHook executes the repo-owned opaque build argv with the
// resolved FLEET_* env exported.
func runBuildHook(ctx context.Context, cfg fleetcfg.Config, namespace, kubecontext string) error {
	if len(cfg.Hooks.Build) == 0 {
		return fmt.Errorf("--build: no hooks.build configured in fleet.yaml")
	}

	return runPassthrough(ctx, "", namespace, kubecontext, cfg.Hooks.Build[0], cfg.Hooks.Build[1:]...)
}

// runPassthrough runs a subprocess with stdio attached and the resolved
// FLEET_* env re-exported (the RFC's bidirectional-variable rule).
func runPassthrough(ctx context.Context, kubeconfig, namespace, kubecontext, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()

	if namespace != "" {
		cmd.Env = append(cmd.Env, "FLEET_NAMESPACE="+namespace)
	}

	if kubecontext != "" {
		cmd.Env = append(cmd.Env, "FLEET_KUBECONTEXT="+kubecontext)
	}

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
