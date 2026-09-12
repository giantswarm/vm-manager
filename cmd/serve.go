package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/apierr"
	"github.com/giantswarm/vm-manager/internal/attest"
	"github.com/giantswarm/vm-manager/internal/host"
	"github.com/giantswarm/vm-manager/internal/images"
	"github.com/giantswarm/vm-manager/internal/imds"
	"github.com/giantswarm/vm-manager/internal/metrics"
	"github.com/giantswarm/vm-manager/internal/network"
	"github.com/giantswarm/vm-manager/internal/runtime/proc"
	"github.com/giantswarm/vm-manager/internal/runtime/qemu"
	"github.com/giantswarm/vm-manager/internal/server"
	"github.com/giantswarm/vm-manager/internal/storage"
	"github.com/giantswarm/vm-manager/internal/tpm"
	"github.com/giantswarm/vm-manager/internal/vm"
)

// systemStateDir is the state directory when vm-manager runs without a home
// directory, e.g. as a systemd system service.
const systemStateDir = "/var/lib/vm-manager"

// Subdirectories of the state dir owned by the command wiring.
const (
	imagesSubdir  = "images"
	volumesSubdir = "volumes"
)

// closeGrace is added to the stop timeout when shutting the VM service down:
// every VM gets its graceful stop, then the escalation must finish too.
const closeGrace = 10 * time.Second

// Values of --attestation.
const (
	// attestationNoop accepts every quote that echoes a nonce; nothing about
	// the TPM is checked and user-data gating is not enforced. Opt-in for
	// hosts without a policy for their image.
	attestationNoop = "noop"
	// attestationVerify, the default, verifies quotes against the image
	// policy (internal/attest). Without golden values in policy.json every
	// quote is rejected until `vm-manager image golden` recorded them from a
	// VM booted with --attestation-learn-golden.
	attestationVerify = "verify"
)

type serveOptions struct {
	listen  string
	mcpPath string

	stateDir       string
	imageDir       string
	networkSubnet  string
	defaultNetwork string
	installTimeout time.Duration
	bootTimeout    time.Duration
	stopTimeout    time.Duration
	ovmfCode       string
	ovmfVars       string
	notifyPort     int
	attestation    string
	learnGolden    bool
	launcher       string
	detachVMs      bool

	metricsEnabled          bool
	metricsGuestSeriesLimit int

	oauthEnabled                  bool
	oauthBaseURL                  string
	oauthProvider                 string
	dexIssuerURL                  string
	dexClientID                   string
	dexClientSecret               string
	dexCAFile                     string
	dexAllowPrivateIP             bool
	googleClientID                string
	googleClientSecret            string
	oauthTrustedAudiences         string
	ssoAllowPrivateIPs            bool
	allowPublicClientRegistration bool
}

func newServeCmd() *cobra.Command {
	o := &serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the REST + MCP server",
		Long: `Run the vm-manager server. Every flag can also be set through the
environment variable named next to it; flags win over the environment.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), o)
		},
	}
	ovmfCode, ovmfVars := vm.FindOVMF()
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", envOr("VM_MANAGER_LISTEN", "127.0.0.1:8080"), "Listen address; loopback by default because the API is anonymous without --enable-oauth (VM_MANAGER_LISTEN)")
	f.StringVar(&o.mcpPath, "mcp-path", envOr("VM_MANAGER_MCP_PATH", "/mcp"), "MCP endpoint path (VM_MANAGER_MCP_PATH)")
	f.StringVar(&o.stateDir, "state-dir", envOr("VM_MANAGER_STATE_DIR", defaultStateDir()), "Directory for VM state, consoles, vTPM state and sockets; created if missing. Default: $XDG_STATE_HOME/vm-manager, else ~/.local/state/vm-manager, else /var/lib/vm-manager without a home directory (VM_MANAGER_STATE_DIR)")
	f.StringVar(&o.imageDir, "image-dir", envOr("VM_MANAGER_IMAGE_DIR", ""), "Directory holding the base images, UKIs and sysext layers; default: <state-dir>/images (VM_MANAGER_IMAGE_DIR)")
	f.StringVar(&o.networkSubnet, "network-subnet", envOr("VM_MANAGER_NETWORK_SUBNET", "192.168.127.0/24"), "CIDR of the default VM network; the gateway is its first address (VM_MANAGER_NETWORK_SUBNET)")
	f.StringVar(&o.defaultNetwork, "default-network", envOr("VM_MANAGER_DEFAULT_NETWORK", api.DefaultNetwork), "Name of the network created at startup from --network-subnet when it does not exist yet; create_vm attaches to it unless told otherwise (VM_MANAGER_DEFAULT_NETWORK)")
	f.DurationVar(&o.installTimeout, "install-timeout", envDuration("VM_MANAGER_INSTALL_TIMEOUT", vm.DefaultInstallTimeout), "How long the installer boot may take before the VM is failed (VM_MANAGER_INSTALL_TIMEOUT)")
	f.DurationVar(&o.bootTimeout, "boot-timeout", envDuration("VM_MANAGER_BOOT_TIMEOUT", vm.DefaultBootTimeout), "How long an installed boot may take to send READY=1 before the VM is reported running instead of ready (VM_MANAGER_BOOT_TIMEOUT)")
	f.DurationVar(&o.stopTimeout, "stop-timeout", envDuration("VM_MANAGER_STOP_TIMEOUT", vm.DefaultStopTimeout), "How long a guest gets to power down before it is killed, also on shutdown (VM_MANAGER_STOP_TIMEOUT)")
	f.StringVar(&o.ovmfCode, "ovmf-code", envOr("VM_MANAGER_OVMF_CODE", ovmfCode), "OVMF firmware code image VMs boot with (VM_MANAGER_OVMF_CODE)")
	f.StringVar(&o.ovmfVars, "ovmf-vars", envOr("VM_MANAGER_OVMF_VARS", ovmfVars), "OVMF variable store template copied per VM (VM_MANAGER_OVMF_VARS)")
	f.IntVar(&o.notifyPort, "notify-port", envInt("VM_MANAGER_NOTIFY_PORT", 0), "vsock port guests send sd_notify messages (READY=1, STATUS=) to; 0 lets the kernel pick one (VM_MANAGER_NOTIFY_PORT)")
	f.StringVar(&o.launcher, "launcher", envOr("VM_MANAGER_LAUNCHER", string(proc.LauncherAuto)), "How QEMU and swtpm are started: systemd runs each as a transient unit (vm-manager-<id>-qemu, -swtpm) under the system manager as root or the user's own manager otherwise, so VMs survive a vm-manager restart; process runs plain children that end with vm-manager; auto picks systemd when a manager is reachable (VM_MANAGER_LAUNCHER)")
	f.BoolVar(&o.detachVMs, "detach-vms-on-exit", envBool("VM_MANAGER_DETACH_VMS_ON_EXIT", true), "Leave running VMs to the next vm-manager on shutdown instead of stopping them; effective with the systemd launcher only, which the next start reattaches to (VM_MANAGER_DETACH_VMS_ON_EXIT)")
	f.BoolVar(&o.metricsEnabled, "metrics-enabled", envBool("VM_MANAGER_METRICS_ENABLED", true), "Serve the Prometheus exposition at GET /metrics, outside the OAuth guard like /healthz: per-VM host metrics and the guests' systemd-report families (VM_MANAGER_METRICS_ENABLED)")
	f.IntVar(&o.metricsGuestSeriesLimit, "metrics-guest-series-limit", envInt("VM_MANAGER_METRICS_GUEST_SERIES_LIMIT", metrics.DefaultMaxGuestSeries), "Series kept per VM from one systemd-report upload; the rest are counted in vm_guest_report_series_dropped_total (VM_MANAGER_METRICS_GUEST_SERIES_LIMIT)")
	f.StringVar(&o.attestation, "attestation", envOr("VM_MANAGER_ATTESTATION", attestationVerify), "How guest TPM quotes are judged: verify checks signature, nonce, PCR digest, PCR 11 against the image's policy.json and PCRs 0, 2-4, 6, 7 (13 at ready) against its golden values, pinning the attestation key per VM; noop accepts any quote with a valid nonce and does not enforce user-data gating (VM_MANAGER_ATTESTATION)")
	f.BoolVar(&o.learnGolden, "attestation-learn-golden", envBool("VM_MANAGER_ATTESTATION_LEARN_GOLDEN", false), "With --attestation=verify, accept golden PCRs that have no value in the image policy and record the observed values on the VM's attestation for `vm-manager image golden`; bring-up of a new image or firmware only: boot one VM with it, run `vm-manager image golden`, restart without (VM_MANAGER_ATTESTATION_LEARN_GOLDEN)")
	f.BoolVar(&o.oauthEnabled, "enable-oauth", envBool("VM_MANAGER_OAUTH_ENABLED", false), "Require an OAuth 2.1 bearer token on the MCP endpoint and the REST API, validated against the platform IdP (mcp-oauth); the caller's identity travels with every request (VM_MANAGER_OAUTH_ENABLED)")
	f.StringVar(&o.oauthBaseURL, "oauth-base-url", envOr("VM_MANAGER_OAUTH_BASE_URL", ""), "Public base URL of this server: the issuer of its OAuth metadata, https or loopback http (VM_MANAGER_OAUTH_BASE_URL)")
	f.StringVar(&o.oauthProvider, "oauth-provider", envOr("VM_MANAGER_OAUTH_PROVIDER", server.ProviderDex), "Identity provider: dex or google (VM_MANAGER_OAUTH_PROVIDER)")
	f.StringVar(&o.dexIssuerURL, "dex-issuer-url", envOr("DEX_ISSUER_URL", ""), "Dex issuer URL (DEX_ISSUER_URL)")
	f.StringVar(&o.dexClientID, "dex-client-id", envOr("DEX_CLIENT_ID", ""), "Dex client ID (DEX_CLIENT_ID)")
	f.StringVar(&o.dexClientSecret, "dex-client-secret", envOr("DEX_CLIENT_SECRET", ""), "Dex client secret (DEX_CLIENT_SECRET)")
	f.StringVar(&o.dexCAFile, "dex-ca-file", envOr("DEX_CA_FILE", ""), "PEM CA bundle of a Dex with a private certificate; verifies discovery, token and JWKS calls (DEX_CA_FILE)")
	f.BoolVar(&o.dexAllowPrivateIP, "allow-private-oauth-urls", envBool("VM_MANAGER_OAUTH_ALLOW_PRIVATE_URLS", false), "Let the Dex issuer resolve to a private or loopback address, an in-cluster Dex (VM_MANAGER_OAUTH_ALLOW_PRIVATE_URLS)")
	f.StringVar(&o.googleClientID, "google-client-id", envOr("GOOGLE_CLIENT_ID", ""), "Google OAuth client ID (GOOGLE_CLIENT_ID)")
	f.StringVar(&o.googleClientSecret, "google-client-secret", envOr("GOOGLE_CLIENT_SECRET", ""), "Google OAuth client secret (GOOGLE_CLIENT_SECRET)")
	f.StringVar(&o.oauthTrustedAudiences, "oauth-trusted-audiences", envOr("OAUTH_TRUSTED_AUDIENCES", ""), "Comma-separated OAuth client IDs whose IdP id_tokens are accepted as bearer tokens — the platform client muster forwards tokens for and the portal logs in with (OAUTH_TRUSTED_AUDIENCES)")
	f.BoolVar(&o.ssoAllowPrivateIPs, "sso-allow-private-ips", envBool("SSO_ALLOW_PRIVATE_IPS", false), "Let the IdP's JWKS endpoint resolve to a private address when validating forwarded tokens (SSO_ALLOW_PRIVATE_IPS)")
	f.BoolVar(&o.allowPublicClientRegistration, "allow-public-client-registration", envBool("VM_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION", false), "Accept unauthenticated dynamic client registration; labs only (VM_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION)")
	return cmd
}

// defaultStateDir is $XDG_STATE_HOME/vm-manager, else ~/.local/state/vm-manager,
// else /var/lib/vm-manager when there is no home directory (a system service).
func defaultStateDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "vm-manager")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return systemStateDir
	}
	return filepath.Join(home, ".local", "state", "vm-manager")
}

// complete fills derived defaults, validates the options and creates the
// directories.
func (o *serveOptions) complete() error {
	if o.imageDir == "" {
		o.imageDir = filepath.Join(o.stateDir, imagesSubdir)
	}
	if _, _, err := net.ParseCIDR(o.networkSubnet); err != nil {
		return fmt.Errorf("--network-subnet: %w", err)
	}
	if o.defaultNetwork == "" {
		return errors.New("--default-network: name required")
	}
	if o.notifyPort < 0 || o.notifyPort > math.MaxUint32 {
		return fmt.Errorf("--notify-port: %d is not a vsock port", o.notifyPort)
	}
	if o.metricsGuestSeriesLimit == 0 {
		o.metricsGuestSeriesLimit = metrics.DefaultMaxGuestSeries
	}
	if o.metricsGuestSeriesLimit < 1 {
		return fmt.Errorf("--metrics-guest-series-limit: %d must be at least 1", o.metricsGuestSeriesLimit)
	}
	if o.attestation == "" {
		o.attestation = attestationVerify
	}
	if o.attestation != attestationNoop && o.attestation != attestationVerify {
		return fmt.Errorf("--attestation: %q is not %s or %s", o.attestation, attestationNoop, attestationVerify)
	}
	if o.learnGolden && o.attestation != attestationVerify {
		return fmt.Errorf("--attestation-learn-golden needs --attestation=%s", attestationVerify)
	}
	if o.launcher == "" {
		o.launcher = string(proc.LauncherAuto)
	}
	if !slices.Contains(proc.LauncherModes, proc.LauncherMode(o.launcher)) {
		return fmt.Errorf("--launcher: %q is not one of %v", o.launcher, proc.LauncherModes)
	}
	for _, dir := range []string{o.stateDir, o.imageDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()
	if err := o.complete(); err != nil {
		return err
	}
	bridgeLogrus(log)

	reg := metrics.New(metrics.Options{
		Version:        version,
		Commit:         buildCommit,
		States:         vm.StateNames(),
		MaxGuestSeries: o.metricsGuestSeriesLimit,
		Logger:         log,
	})
	c, err := newComponents(ctx, o, reg, log)
	if err != nil {
		return err
	}
	reg.SetSource(c.vm)

	svc := api.Services{Host: host.New(host.Options{Logger: log}), VM: c.vm, Images: c.images, Metrics: reg}
	cfg := server.Config{Addr: o.listen, MCPPath: o.mcpPath}
	if o.metricsEnabled {
		cfg.Metrics = reg.Handler()
	}
	if o.oauthEnabled {
		cfg.OAuth = &server.OAuthConfig{
			BaseURL:                       o.oauthBaseURL,
			Provider:                      o.oauthProvider,
			DexIssuerURL:                  o.dexIssuerURL,
			DexClientID:                   o.dexClientID,
			DexClientSecret:               o.dexClientSecret,
			DexCAFile:                     o.dexCAFile,
			DexAllowPrivateIP:             o.dexAllowPrivateIP,
			GoogleClientID:                o.googleClientID,
			GoogleClientSecret:            o.googleClientSecret,
			TrustedAudiences:              splitList(o.oauthTrustedAudiences),
			SSOAllowPrivateIPs:            o.ssoAllowPrivateIPs,
			AllowPublicClientRegistration: o.allowPublicClientRegistration,
		}
	}
	srv, err := server.New(cfg, svc, api.NewMCPServer(svc, version), log)
	if err != nil {
		return errors.Join(err, c.close(log))
	}
	log.Info("vm-manager starting", "version", version, "listen", o.listen, "rest", api.Prefix, "mcp", o.mcpPath,
		"stateDir", o.stateDir, "imageDir", o.imageDir, "oauth", o.oauthEnabled, "metrics", o.metricsEnabled, "attestation", o.attestation)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = srv.Run(ctx)
	// The listener is down: leave the VMs to the next vm-manager or stop
	// them (--detach-vms-on-exit), then the networks and the notify
	// listener.
	return errors.Join(err, c.close(log))
}

// components are the host services behind the VM service, built in the
// order internal/vm documents: storage, networks, notify, swtpm and QEMU
// runtimes, images, then the service itself.
type components struct {
	storage  storage.Provider
	networks *network.Manager
	// notify is nil when AF_VSOCK is unavailable.
	notify      *qemu.NotifyListener
	images      *images.Catalog
	vm          *vm.Service
	stopTimeout time.Duration
}

func newComponents(ctx context.Context, o *serveOptions, reg *metrics.Registry, log *slog.Logger) (_ *components, err error) {
	// c is a local, not the named result: the error returns below hand back
	// nil, and the deferred close must still see the partially built set.
	c := &components{stopTimeout: o.stopTimeout}
	defer func() {
		if err != nil {
			err = errors.Join(err, c.close(log))
		}
	}()

	c.storage = storage.Detect(ctx, log, filepath.Join(o.stateDir, volumesSubdir))
	log.Info("storage provider selected", "provider", c.storage.Name())

	c.networks = network.NewManager(o.stateDir, log)

	// The port is part of the state dir contract: guests carry it in their
	// vmm.notify_socket credential from boot, so a restarted vm-manager
	// must listen on the port the earlier one recorded.
	var notifier vm.Notifier = noNotify{}
	notify, err := qemu.ListenNotifyPersistent(qemu.NotifyPortFile(o.stateDir), uint32(o.notifyPort), log) // #nosec G115 -- range checked in complete.
	switch {
	case errors.Is(err, qemu.ErrNotifyPortInUse):
		return nil, err
	case err != nil:
		log.Warn("vsock notify listener unavailable: guests cannot report READY=1, VMs settle in state running instead of ready", "err", err)
	default:
		c.notify, notifier = notify, notify
		log.Info("notify listener bound", "vsockPort", notify.Port(), "recordedIn", qemu.NotifyPortFile(o.stateDir))
	}

	c.images, err = images.Load(o.imageDir, log)
	if err != nil {
		return nil, fmt.Errorf("load images: %w", err)
	}
	if n := len(c.images.List()); n == 0 {
		log.Warn("no images in the image directory: create_vm fails until one is built (make -C images) or --image-dir points at a build", "imageDir", o.imageDir)
	} else {
		log.Info("image catalog loaded", "imageDir", o.imageDir, "images", n)
	}

	attestor, err := c.attestor(o, log)
	if err != nil {
		return nil, err
	}
	launcher, err := proc.SelectLauncher(proc.LauncherMode(o.launcher), log)
	if err != nil {
		return nil, fmt.Errorf("--launcher: %w", err)
	}
	detach := o.detachVMs && launcher.Persistent()
	log.Info("vm launcher selected", "launcher", launcher.Mode(), "manager", launcher.Manager, "reason", launcher.Reason, "detachOnExit", detach)
	if !launcher.Persistent() {
		log.Warn("VMs run as child processes and end with vm-manager; a restart marks them stopped. Run under systemd (a user manager or as root) for VMs that survive restarts")
	}
	c.vm, err = vm.New(vm.Options{
		StateDir:         o.stateDir,
		Images:           c.images,
		Storage:          c.storage,
		TPM:              vm.TPM(tpm.New(tpm.Options{Exec: launcher.Exec, Logger: log})),
		Runtime:          vm.QEMURuntime(qemu.New(qemu.Options{Exec: launcher.Exec, Logger: log, OVMFVarsTemplate: o.ovmfVars})),
		Networks:         vm.Networks(c.networks),
		Notify:           notifier,
		Attestor:         attestor,
		OVMFCode:         o.ovmfCode,
		OVMFVarsTemplate: o.ovmfVars,
		InstallTimeout:   o.installTimeout,
		BootTimeout:      o.bootTimeout,
		StopTimeout:      o.stopTimeout,
		DetachOnClose:    detach,
		Logger:           log,
		Metrics:          reg,
	})
	if err != nil {
		return nil, err
	}
	if err := c.vm.Load(ctx); err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	if err := c.ensureDefaultNetwork(ctx, o.defaultNetwork, o.networkSubnet, log); err != nil {
		return nil, err
	}
	return c, nil
}

// attestor builds the imds.Attestor for --attestation. The verifier reads
// each VM's image policy from the VM service, which is built after it, so
// the provider resolves c.vm at call time (quotes only arrive once a VM
// boots, long after both exist).
func (c *components) attestor(o *serveOptions, log *slog.Logger) (imds.Attestor, error) {
	if o.attestation != attestationVerify {
		log.Warn("attestation=noop: guest quotes are not verified and user-data gating is not enforced; run with --attestation=verify")
		return &imds.NoopAttestor{}, nil
	}
	if o.learnGolden {
		log.Warn("attestation-learn-golden: golden PCRs without a value in the image policy are accepted and recorded; disable once `vm-manager image golden` ran")
	}
	return attest.New(attest.Options{
		Policies: attest.PolicyProviderFunc(func(ctx context.Context, vmID string) (attest.Policy, error) {
			return c.vm.PolicyFor(ctx, vmID)
		}),
		LearnGolden: o.learnGolden,
		Logger:      log,
	})
}

// ensureDefaultNetwork creates the default network unless the state dir
// already holds one of that name; a restored network keeps its subnet.
func (c *components) ensureDefaultNetwork(ctx context.Context, name, cidr string, log *slog.Logger) error {
	n, err := c.vm.GetNetwork(name)
	switch {
	case errors.Is(err, apierr.ErrNotFound):
		n, err = c.vm.CreateNetwork(ctx, network.Spec{Name: name, CIDR: cidr, EnableIMDS: true})
		if err != nil {
			return fmt.Errorf("create default network %q: %w", name, err)
		}
		log.Info("default network created", "network", name, "cidr", n.Spec.CIDR, "gateway", n.Gateway)
	case err != nil:
		return fmt.Errorf("default network %q: %w", name, err)
	case n.Spec.CIDR != cidr:
		log.Warn("default network restored with a different subnet than --network-subnet; delete it to apply the flag", "network", name, "cidr", n.Spec.CIDR, "flag", cidr, "gateway", n.Gateway)
	default:
		log.Info("default network restored", "network", name, "cidr", n.Spec.CIDR, "gateway", n.Gateway)
	}
	return nil
}

// close ends the VM service (detaching or stopping the VMs within the stop
// timeout), then the networks and the notify listener. Safe on a partially
// built set.
func (c *components) close(log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.stopTimeout+closeGrace)
	defer cancel()
	var errs []error
	if c.vm != nil {
		if err := c.vm.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("close vm service: %w", err))
		}
	}
	if c.networks != nil {
		if err := c.networks.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close networks: %w", err))
		}
	}
	if c.notify != nil {
		if err := c.notify.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close notify listener: %w", err))
		}
	}
	if len(errs) > 0 {
		log.Warn("shutdown finished with errors", "errors", errors.Join(errs...))
	}
	return errors.Join(errs...)
}

// noNotify stands in for the vsock listener on hosts without AF_VSOCK.
// Subscribe returns a nil channel, which never delivers, so the VM service
// times out into state running; Credential is empty, so guests get no
// notify socket.
type noNotify struct{}

func (noNotify) Credential() string                        { return "" }
func (noNotify) Subscribe(uint32) <-chan qemu.Notification { return nil }
func (noNotify) Unsubscribe(uint32)                        {}
