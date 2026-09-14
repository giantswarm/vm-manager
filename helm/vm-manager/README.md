# vm-manager

VM provisioning service for the Agent Platform — KVM virtual machines with an instance metadata service, a vTPM with measured boot, an immutable image and attestation, exposed as REST and MCP

vm-manager as a platform pod: the Deployment runs privileged on a KVM node with
`/dev/kvm` and `/dev/vhost-vsock` mounted from the node, QEMU and swtpm as its
children (`--launcher process`), the state directory on an optional claim and
the image directory (`make -C images` in a vm-manager checkout) mounted from a
claim or a node path. With `oauth.enabled` it is an OAuth 2.1 resource server
against the platform identity (`global.identity`), and with
`muster.mcpServer.enabled` it registers with muster under the `agent-platform`
tool group, next to agent-manager and model-manager. The agent-platform meta
chart installs it as `components.vm-manager`.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| global | object | `{}` | Platform-wide values an umbrella chart (agent-platform) shares with every component; Helm forwards them to this chart. `oauth.*` reads the identity contract as its defaults: `global.identity.issuerUrl`, `global.identity.clientId`, `global.identity.existingSecret`, `global.identity.ca.secretName` / `.key`, and `global.domain` for the OAuth base URL. Empty here; a standalone install sets `oauth.*` directly. |
| replicaCount | int | `1` | Number of replicas. One: the VMs are children of the one process and the state directory is its own. The Deployment rolls with Recreate for the same reason. |
| image.registry | string | `"ghcr.io"` | Image registry. |
| image.repository | string | `"giantswarm/vm-manager"` | Image repository. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.tag | string | `""` | Image tag. Defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets. |
| nameOverride | string | `""` | Override the chart name. |
| fullnameOverride | string | `""` | Override the fully qualified release name (the umbrella chart pins the Service name through this). |
| host.devices | object | `{"enabled":true,"paths":["/dev/kvm","/dev/vhost-vsock"]}` | The node's KVM devices, mounted into the pod from the node as hostPath character devices: /dev/kvm (hardware virtualization) and /dev/vhost-vsock (the guests' READY=1 and ssh). Off, the pod starts without them and GET /api/v1/host reports them under `missing` — for a cluster without KVM nodes, and for the chart's own install smoke. |
| persistence.existingClaim | string | `""` | Keep the state directory (VM records, disks, vTPM state, consoles) on a PersistentVolumeClaim: an existing claim's name, or `create: true` to render one from the settings below. With neither the state is an emptyDir and the VMs die with the pod — they end with the process anyway (--launcher process), but a claim keeps their records and disks for the next `start_vm`. |
| persistence.create | bool | `false` |  |
| persistence.size | string | `"50Gi"` |  |
| persistence.storageClass | string | `""` |  |
| persistence.accessModes[0] | string | `"ReadWriteOnce"` |  |
| images.existingClaim | string | `""` | The image directory `--image-dir` reads (README "Images"): the base image, its UKI and the Kubernetes sysext layers `make -C images` in a vm-manager checkout builds, with the policy.json that carries the golden PCR values. Mounted from an existing PersistentVolumeClaim an operator fills, or from a node path (a lab: agentlab mounts its checkout's images/build into the kind node); neither means an empty emptyDir and no bootable image. `vm-manager image golden` writes policy.json from outside the pod, so the mount is read-only by default. |
| images.hostPath | string | `""` |  |
| images.readOnly | bool | `true` |  |
| vm.networkSubnet | string | `"192.168.127.0/24"` | CIDR of the default network, created at startup when missing. |
| vm.defaultNetwork | string | `"default"` | Its name; `create_vm` attaches to it unless told otherwise. |
| vm.installTimeout | string | `"5m"` | Installer boot ceiling, then `failed`. |
| vm.bootTimeout | string | `"4m"` | `READY=1` ceiling, then `running`; raise it when user-data runs `kubeadm init` with image pulls. |
| vm.stopTimeout | string | `"30s"` | Graceful power-down before SIGKILL, also on shutdown. |
| vm.attestation | string | `"verify"` | How guest TPM quotes are judged: `verify` (against the image policy and its golden values) or `noop`. |
| vm.learnGolden | bool | `false` | Bring-up of a new image or firmware only: accept golden PCRs the image policy has no value for and record them for `vm-manager image golden`. |
| vm.ovmf.code | string | `""` | OVMF firmware code image and variable store template; empty probes the known locations (the image ships Ubuntu's under /usr/share/OVMF). |
| vm.ovmf.vars | string | `""` |  |
| metrics.enabled | bool | `true` | Serve the Prometheus exposition at GET /metrics, outside the OAuth guard like /healthz. |
| metrics.guestSeriesLimit | int | `1000` | Series kept per VM from one systemd-report upload. |
| mcp.path | string | `"/mcp"` | MCP endpoint path (the REST API is always served). |
| oauth.enabled | bool | `false` | Make vm-manager an OAuth 2.1 resource server (mcp-oauth): the MCP endpoint and the REST API require a bearer token the platform identity provider issued, and every call carries the caller's identity. On the Agent Platform muster forwards the session's IdP id_token to this server (MCPServer `auth.forwardToken`, rendered below), validated against the IdP's JWKS when its audience is in `trustedAudiences`. Off: anonymous — only for a server nothing but a trusted proxy can reach. |
| oauth.baseURL | string | `""` | Public base URL of this server: the issuer of its own OAuth metadata (https, or http on loopback). Empty derives `https://<fullname>.<global.domain>` when `global.domain` is set. |
| oauth.provider | string | `"dex"` | Identity provider: `dex` or `google`. |
| oauth.dex.issuerURL | string | `""` | Dex issuer URL. Empty falls back to `global.identity.issuerUrl`. |
| oauth.dex.clientID | string | `""` | Dex OAuth client ID. Empty falls back to `global.identity.clientId`. |
| oauth.dex.clientSecret | string | `""` | Dex OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.dex.allowPrivateURLs | bool | `false` | Let the issuer resolve to a private or loopback address (an in-cluster Dex). |
| oauth.dex.caSecret | object | `{"key":"ca.crt","name":""}` | Secret with the CA of a Dex that serves a private certificate; mounted and passed as `--dex-ca-file`. Empty name falls back to `global.identity.ca.secretName` / `global.identity.ca.key`. |
| oauth.google.clientID | string | `""` | Google OAuth client ID (not secret; may also come from the Secret key `google-client-id` when empty). |
| oauth.google.clientSecret | string | `""` | Google OAuth client secret (prefer `oauth.existingSecret`). |
| oauth.existingSecret | string | `""` | Existing Secret with the provider credentials: `dex-client-secret` (dex) or `google-client-secret` (+ optional `google-client-id`) (google). Empty falls back to `global.identity.existingSecret`, whose `dex-client-secret` is the platform client's; without that, the chart renders a Secret from the values above. |
| oauth.trustedAudiences | list | `[]` | OAuth client IDs whose IdP id_tokens are accepted as bearer tokens (SSO token forwarding). Empty falls back to `[global.identity.clientId]`, the platform client MCP clients and the muster CLI log in with. The server trusts the union of this list and `muster.mcpServer.auth.requiredAudiences` (in that order, without duplicates). |
| oauth.sso.allowPrivateIPs | bool | `false` | Let the IdP's JWKS endpoint resolve to a private address when validating forwarded tokens (an in-cluster Dex). |
| oauth.allowPublicClientRegistration | bool | `false` | Accept unauthenticated dynamic client registration (labs only). |
| muster.mcpServer.enabled | bool | `false` | Register this server with muster by rendering an `mcpservers.muster.giantswarm.io` CR in the release namespace. Tools then appear as `x_<name>_<tool>`. The CR carries `agent-platform.giantswarm.io/tool-group: agent-platform`: the platform's own management surface, next to agent-manager and model-manager. |
| muster.mcpServer.name | string | `"vm-manager"` | MCPServer CR name (drives the tool prefix). |
| muster.mcpServer.autoStart | bool | `true` | Start the server connection when muster initializes. |
| muster.mcpServer.description | string | `"VM provisioning on the platform's KVM node (images, networks, VMs with measured boot and attestation); call get_host first, then list_images and list_networks, then create_vm"` | Human-readable description shown by muster. |
| muster.mcpServer.labels | object | `{}` | Extra labels on the MCPServer CR (merged last: they may override the tool-group label). |
| muster.mcpServer.auth | object | `{"forwardToken":true,"requiredAudiences":[]}` | How muster authenticates to this server; rendered only with `oauth.enabled`. `forwardToken` makes muster forward the session's IdP id_token byte-identical. `requiredAudiences` are extra audiences that token must carry, trusted as bearer audiences by construction. |
| networkPolicy.enabled | bool | `false` | Create a Kubernetes NetworkPolicy for the pod. |
| networkPolicy.ingressNamespaces | list | `[]` | Namespaces allowed to reach the API (label kubernetes.io/metadata.name). Empty allows ingress from the release namespace only. |
| networkPolicy.guestEgress | bool | `true` | The guests' traffic leaves the pod: the networks are userspace NAT (gvisor-tap-vsock) and a VM reaches whatever the pod reaches, so the policy admits every egress destination. False limits egress to DNS and the identity provider — VMs then have no network beyond their own. |
| serviceAccount.create | bool | `true` | Create a ServiceAccount. vm-manager calls no Kubernetes API; the token is never mounted. |
| serviceAccount.annotations | object | `{}` | Annotations on the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name (generated when empty). |
| podAnnotations | object | `{}` | Annotations on the pod. |
| podLabels | object | `{}` | Labels on the pod. |
| podSecurityContext | object | `{}` | Pod security context. Root: /dev/kvm is root:kvm on every node and the process forks QEMU and swtpm as its own children. |
| securityContext | object | `{"privileged":true,"runAsGroup":0,"runAsUser":0}` | Container security context. Privileged, on purpose: a hostPath device mounted into an unprivileged container is denied by the device cgroup (open(2) fails with EPERM) unless a device plugin hands it out, so the privileged flag is what makes /dev/kvm and /dev/vhost-vsock usable. No capability is used beyond that (no bridges, no tap devices, no CAP_NET_ADMIN: the networks are userspace). |
| service.type | string | `"ClusterIP"` | Service type. |
| service.port | int | `8080` | Service port (container listens on 8080). |
| resources | object | `{"requests":{"cpu":"250m","memory":"512Mi"}}` | Container resources. The VMs are QEMU processes inside this container, so a memory limit bounds the sum of their memory too; none by default. |
| logging.verbose | bool | `false` | Enable debug logging. |
| extraArgs | list | `[]` | Extra container arguments. |
| extraEnv | list | `[]` | Extra environment variables. |
| nodeSelector | object | `{}` | Node selector: the KVM node(s) — a label such as `kvm.giantswarm.io/enabled: "true"` on nodes that expose /dev/kvm. |
| tolerations | list | `[]` | Tolerations. |
| affinity | object | `{}` | Affinity. |
