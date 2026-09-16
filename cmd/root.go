// Package cmd holds the vm-manager CLI.
package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/giantswarm/vm-manager/internal/buildinfo"
)

// build is the identity main resolved from the ldflags and the Go build
// info: the release version (dev for an untagged local build), the commit
// and the build time.
var build = buildinfo.Info{Version: buildinfo.DevVersion, Commit: buildinfo.UnknownCommit, Date: buildinfo.UnknownDate}

// SetBuild records the build identity the CLI, the MCP server and the metrics
// report.
func SetBuild(b buildinfo.Info) { build = b }

func newRootCmd() *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:   "vm-manager",
		Short: "Systemd-native VM provisioner for the Agent Platform",
		Long: `vm-manager provisions cloud-provider-like virtual machines on a KVM host:
an instance metadata service, a vTPM with measured boot, an immutable
mkosi-built OS and Kubernetes as a sysext layer, so agents can hand the VMs
to the CAPI based cluster-manager. The API is served as REST/JSON and as MCP
tools from one process.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	root.Version = build.Version
	root.SetVersionTemplate("vm-manager version {{.Version}}\n")
	root.AddCommand(newServeCmd(), newImageCmd(), newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "vm-manager version %s\n  commit: %s\n  built:  %s\n", build.Version, build.Commit, build.Date)
		},
	}
}

// Execute runs the CLI.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
