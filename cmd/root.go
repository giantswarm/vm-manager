// Package cmd holds the vm-manager CLI.
package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	version     = "dev"
	buildCommit = "unknown"
	buildDate   = "unknown"
)

// SetVersion records the build version (set from main via ldflags).
func SetVersion(v string) { version = v }

// SetBuildInfo records the commit and build date.
func SetBuildInfo(commit, date string) {
	buildCommit = commit
	buildDate = date
}

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
	root.Version = version
	root.SetVersionTemplate("vm-manager version {{.Version}}\n")
	root.AddCommand(newServeCmd(), newImageCmd(), newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "vm-manager version %s\n  commit: %s\n  built:  %s\n", version, buildCommit, buildDate)
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
