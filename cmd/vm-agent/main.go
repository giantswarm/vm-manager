// vm-agent is the guest binary of vm-manager's attestation protocol
// (docs/design.md "Boot flow" steps 5 and 7). It is built fully static
// (make agent) and shipped in the initrd and the root file system of the
// guest image; it speaks to the vTPM at /dev/tpmrm0 and to the IMDS at
// http://169.254.169.254/giantswarm/v1.
//
// # Subcommands
//
//	vm-agent attest --stage=initrd|ready [--imds-url=] [--tpm=] [--timeout=90s] [--print] [--nonce=]
//	vm-agent pcrs [--bank=sha256] [--tpm=]
//	vm-agent version
//
// attest fetches a nonce, quotes the stage's PCRs with the persistent AK
// (internal/agent/quote) and posts the quote. It exits 0 when the verifier
// accepted the quote, 2 when it rejected it (the verifier's message is
// printed) and 1 on every other error. Requests are retried with backoff
// while the IMDS is not reachable yet, up to --timeout. --print writes the
// request as JSON to stdout instead of posting it; --nonce uses the given
// hex nonce instead of fetching one, so a quote can be produced without an
// IMDS. pcrs prints PCR 0-23 of one bank for debugging.
//
// # How the image runs it
//
// The initrd ships vm-agent-attest.service (images/mkosi.initrd.conf),
// wanted by initrd.target, After=network-online.target and
// systemd-pcrphase-initrd.service, Before=ignition-fetch.service, running
// `vm-agent attest --stage=initrd --timeout=90s`; until that quote verifies,
// the IMDS answers /user-data with 503 + Retry-After and Ignition's fetch
// stage keeps retrying. The root file system ships the same unit name
// (images/mkosi.images/base) After=vm-kubernetes.service and
// systemd-pcrphase.service, Before=multi-user.target, running `vm-agent
// attest --stage=ready`, so the quote covers the Kubernetes sysext
// measurement (PCR 13) and the full PCR 11 phase path and precedes READY=1.
// Both units are Type=oneshot without Restart= or OnFailure=: a rejection
// (exit 2) is a policy decision the host has logged and re-quoting the same
// PCRs cannot change it; the unit fails visibly and the boot goes on (with
// require_attestation the fetch stage then times out into emergency.target).
// The agent itself retries the IMDS with backoff until --timeout.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/vm-manager/internal/agent"
	"github.com/giantswarm/vm-manager/internal/agent/quote"
	"github.com/giantswarm/vm-manager/internal/imds"
)

// Set by the build via ldflags (-X main.version=...).
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// Exit codes of attest.
const (
	exitOK       = 0
	exitError    = 1
	exitRejected = 2
)

// buildFunc is quote.Build, injected so the command can be tested without a
// TPM.
type buildFunc func(opts quote.Options, stage imds.Stage, nonce string) (imds.QuoteRequest, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, quote.Build)
	stop()
	os.Exit(code)
}

// run executes the CLI and maps its outcome to an exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, build buildFunc) int {
	root := newRootCmd(stderr, build)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, agent.ErrRejected):
		_, _ = fmt.Fprintln(stderr, "Attestation rejected:", err)
		return exitRejected
	default:
		_, _ = fmt.Fprintln(stderr, "Error:", err)
		return exitError
	}
}

func newRootCmd(stderr io.Writer, build buildFunc) *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:           "vm-agent",
		Short:         "Guest agent of vm-manager: TPM attestation against the IMDS",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	root.Version = version
	root.SetVersionTemplate("vm-agent version {{.Version}}\n")
	root.AddCommand(newAttestCmd(build), newPCRsCmd(), newVersionCmd())
	return root
}

func newAttestCmd(build buildFunc) *cobra.Command {
	var (
		stage   string
		imdsURL string
		tpmDev  string
		timeout time.Duration
		print   bool
		nonce   string
	)
	cmd := &cobra.Command{
		Use:   "attest --stage=initrd|ready",
		Short: "Quote the stage's PCRs and post the quote to the IMDS",
		Long: `attest fetches a nonce from the IMDS, quotes PCRs 0-7 and 11 (stage initrd)
or 0-7, 11 and 13 (stage ready) with the persistent attestation key and posts
the quote. Exit 0: verified; 2: rejected by the verifier; 1: any other error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := quote.Options{Device: tpmDev}
			cfg := agent.Config{
				IMDSURL:   imdsURL,
				Stage:     imds.Stage(stage),
				Nonce:     nonce,
				Timeout:   timeout,
				UserAgent: "vm-agent/" + version,
				Quote: func(stage imds.Stage, nonce string) (imds.QuoteRequest, error) {
					return build(opts, stage, nonce)
				},
			}
			if print {
				cfg.Print = cmd.OutOrStdout()
			}
			_, err := agent.Attest(cmd.Context(), cfg)
			return err
		},
	}
	cmd.Flags().StringVar(&stage, "stage", "", "Boot stage being quoted: initrd or ready (required)")
	cmd.Flags().StringVar(&imdsURL, "imds-url", agent.DefaultIMDSURL, "IMDS base URL")
	cmd.Flags().StringVar(&tpmDev, "tpm", quote.DefaultDevice, "TPM device")
	cmd.Flags().DurationVar(&timeout, "timeout", agent.DefaultTimeout, "Overall timeout, including the wait for the IMDS")
	cmd.Flags().BoolVar(&print, "print", false, "Print the quote request as JSON instead of posting it")
	cmd.Flags().StringVar(&nonce, "nonce", "", "Use this hex nonce instead of fetching one from the IMDS")
	_ = cmd.MarkFlagRequired("stage")
	return cmd
}

func newPCRsCmd() *cobra.Command {
	var (
		bank   string
		tpmDev string
	)
	cmd := &cobra.Command{
		Use:   "pcrs",
		Short: "Print PCR 0-23 of one bank",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			alg, err := quote.BankAlg(bank)
			if err != nil {
				return err
			}
			tpm, err := quote.Open(quote.Options{Device: tpmDev})
			if err != nil {
				return err
			}
			defer func() { _ = tpm.Close() }()
			values, err := quote.ReadPCRs(tpm, alg, quote.AllPCRs())
			if err != nil {
				return fmt.Errorf("read pcrs: %w", err)
			}
			for _, idx := range quote.AllPCRs() {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%2d: %x\n", idx, values[idx]); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&bank, "bank", quote.Bank, "PCR bank: sha1, sha256, sha384 or sha512")
	cmd.Flags().StringVar(&tpmDev, "tpm", quote.DefaultDevice, "TPM device")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "vm-agent version %s\n  commit: %s\n  built:  %s\n", version, commit, date)
		},
	}
}
