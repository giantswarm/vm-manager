package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/giantswarm/vm-manager/internal/api"
	"github.com/giantswarm/vm-manager/internal/host"
	"github.com/giantswarm/vm-manager/internal/server"
)

// systemStateDir is the state directory when vm-manager runs without a home
// directory, e.g. as a systemd system service.
const systemStateDir = "/var/lib/vm-manager"

type serveOptions struct {
	listen  string
	mcpPath string

	stateDir      string
	imageDir      string
	networkSubnet string

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
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", envOr("VM_MANAGER_LISTEN", "127.0.0.1:8080"), "Listen address; loopback by default because the API is anonymous without --enable-oauth (VM_MANAGER_LISTEN)")
	f.StringVar(&o.mcpPath, "mcp-path", envOr("VM_MANAGER_MCP_PATH", "/mcp"), "MCP endpoint path (VM_MANAGER_MCP_PATH)")
	f.StringVar(&o.stateDir, "state-dir", envOr("VM_MANAGER_STATE_DIR", defaultStateDir()), "Directory for VM state, consoles, vTPM state and sockets; created if missing. Default: $XDG_STATE_HOME/vm-manager, else ~/.local/state/vm-manager, else /var/lib/vm-manager without a home directory (VM_MANAGER_STATE_DIR)")
	f.StringVar(&o.imageDir, "image-dir", envOr("VM_MANAGER_IMAGE_DIR", ""), "Directory holding the base images, UKIs and sysext layers; default: <state-dir>/images (VM_MANAGER_IMAGE_DIR)")
	f.StringVar(&o.networkSubnet, "network-subnet", envOr("VM_MANAGER_NETWORK_SUBNET", "192.168.127.0/24"), "CIDR of the default VM network; the gateway is its first address (VM_MANAGER_NETWORK_SUBNET)")
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

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()
	if o.imageDir == "" {
		o.imageDir = filepath.Join(o.stateDir, "images")
	}
	if _, _, err := net.ParseCIDR(o.networkSubnet); err != nil {
		return fmt.Errorf("--network-subnet: %w", err)
	}
	for _, dir := range []string{o.stateDir, o.imageDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	svc := api.Services{Host: host.New(host.Options{Logger: log})}

	cfg := server.Config{Addr: o.listen, MCPPath: o.mcpPath}
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
		return err
	}
	log.Info("vm-manager starting", "version", version, "listen", o.listen, "rest", api.Prefix, "mcp", o.mcpPath,
		"stateDir", o.stateDir, "imageDir", o.imageDir, "networkSubnet", o.networkSubnet, "oauth", o.oauthEnabled)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
}
