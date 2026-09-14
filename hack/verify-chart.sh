#!/usr/bin/env bash
# Render assertions for helm/vm-manager: rules the templates encode that
# `helm lint` and the values schema cannot check. Runs in the chart workflow
# and as `make helm-verify`. Needs helm.
set -euo pipefail
cd "$(dirname "$0")/.."
CHART=helm/vm-manager

fail() { echo "verify-chart: FAIL: $*" >&2; exit 1; }

render() { helm template vmm "$CHART" "$@"; }
deployment() { render --show-only templates/deployment.yaml "$@"; }
args() { deployment "$@" | { grep -o -- '--[a-z-]*=[^ ]*' || true; }; }

# A Dex install that sets only the platform identity contract.
DEX=(--set oauth.enabled=true --set global.domain=example.test
     --set global.identity.issuerUrl=https://dex.example.test
     --set global.identity.clientId=platform-client
     --set global.identity.existingSecret=idp
     --set global.identity.ca.secretName=dex-ca)

# The pod shape: privileged (the runtime hands a privileged container the
# node's devices), nothing mounted from the node, the process launcher, the
# state and image directories.
got=$(args)
for flag in '--launcher=process' '--state-dir=/var/lib/vm-manager' '--image-dir=/var/lib/vm-manager/images' '--listen=:8080' '--attestation=verify'; do
  echo "$got" | grep -qx -- "$flag" || fail "default render lacks $flag, got: $got"
done
deployment | grep -q '^ *privileged: true$' || fail "default render is not privileged"
if render | grep -q 'hostPath'; then
  fail "the chart mounts a hostPath: component inputs are artifacts and volumes, never node paths"
fi
deployment | grep -q '^ *type: Recreate$' || fail "the Deployment must roll with Recreate"
deployment | grep -q 'automountServiceAccountToken: false' || fail "the ServiceAccount token must not be mounted"

# The guest image: an init container pulls the artifact of the appVersion by
# default, a tag or a digest when set, over plain HTTP for a lab registry;
# off, no init container.
# The init container's block of the rendered pod spec (from its name to its
# volume mounts), so assertions cannot match the server container.
initc() { deployment "$@" | sed -n '/^ *- name: guest-image$/,/^ *volumeMounts:$/p'; }
initc | grep -qx ' *- pull' || fail "no guest-image init container running image pull"
appversion=$(grep '^appVersion:' "$CHART/Chart.yaml" | cut -d: -f2 | tr -d ' "')
initc | grep -qx " *- gsoci.azurecr.io/giantswarm/vm-manager-guest-image:$appversion" || fail "guest image reference is not the appVersion's ($appversion)"
initc --set guestImage.tag=1.2.3 | grep -qx ' *- gsoci.azurecr.io/giantswarm/vm-manager-guest-image:1.2.3' || fail "guestImage.tag is not used"
initc --set guestImage.digest=sha256:0123 | grep -qx ' *- gsoci.azurecr.io/giantswarm/vm-manager-guest-image@sha256:0123' || fail "guestImage.digest is not used"
initc --set guestImage.registry=agentlab-registry:5000 --set guestImage.repository=vm-manager-guest-image --set guestImage.plainHTTP=true | grep -qx ' *- --plain-http' || fail "guestImage.plainHTTP renders no --plain-http"
initc | grep -qx ' *- --image-dir=/var/lib/vm-manager/images' || fail "the init container pulls into another directory than the server reads"
if initc | grep -q 'privileged: true'; then
  fail "the init container must not be privileged"
fi
if deployment --set guestImage.enabled=false | grep -q 'initContainers'; then
  fail "guestImage.enabled=false still renders an init container"
fi
deployment --set guestImage.pullSecret=mirror-creds | grep -q 'secretName: mirror-creds' || fail "guestImage.pullSecret is not mounted"
deployment --set guestImage.pullSecret=mirror-creds | grep -q 'name: DOCKER_CONFIG' || fail "guestImage.pullSecret sets no DOCKER_CONFIG"

# The state directory: emptyDir by default, the named claim, the chart's own
# claim with persistence.create; the init container writes the same volume.
deployment | grep -A1 'name: state$' | grep -q 'emptyDir' || fail "default state volume is not an emptyDir"
[ "$(deployment | grep -c 'mountPath: /var/lib/vm-manager$')" = 2 ] || fail "the state volume must be mounted in the init container and the server"
deployment --set persistence.existingClaim=vm-state | grep -q 'claimName: vm-state' || fail "persistence.existingClaim is not mounted"
render --set persistence.create=true --show-only templates/state-pvc.yaml | grep -q 'name: vmm-vm-manager-state' || fail "persistence.create renders no claim"
deployment --set persistence.create=true | grep -q 'claimName: vmm-vm-manager-state' || fail "persistence.create is not mounted"

# OAuth from the identity contract: issuer, client, CA file and trusted
# audiences = union(oauth.trustedAudiences | default [global.identity.clientId],
# muster.mcpServer.auth.requiredAudiences), de-duplicated, order stable.
got=$(args "${DEX[@]}")
echo "$got" | grep -qx -- '--dex-issuer-url=https://dex.example.test' || fail "issuer from global.identity: got '$got'"
echo "$got" | grep -qx -- '--dex-client-id=platform-client' || fail "client id from global.identity: got '$got'"
echo "$got" | grep -qx -- '--dex-ca-file=/etc/vm-manager/idp-ca/ca.crt' || fail "CA file from global.identity.ca: got '$got'"
echo "$got" | grep -qx -- '--oauth-trusted-audiences=platform-client' || fail "trusted audiences from global.identity alone: got '$got'"
echo "$got" | grep -qx -- '--oauth-base-url=https://vmm-vm-manager.example.test' || fail "base URL from global.domain: got '$got'"
got=$(args "${DEX[@]}" --set-json 'muster.mcpServer.auth.requiredAudiences=["kubernetes"]' | grep -- '--oauth-trusted-audiences=' | cut -d= -f2-)
[ "$got" = "platform-client,kubernetes" ] || fail "global.identity + requiredAudiences: want platform-client,kubernetes, got '$got'"
got=$(args "${DEX[@]}" --set-json 'oauth.trustedAudiences=["portal","kubernetes"]' \
  --set-json 'muster.mcpServer.auth.requiredAudiences=["kubernetes","dex-k8s-authenticator"]' | grep -- '--oauth-trusted-audiences=' | cut -d= -f2-)
[ "$got" = "portal,kubernetes,dex-k8s-authenticator" ] || fail "explicit list + requiredAudiences, de-duplicated: got '$got'"
deployment "${DEX[@]}" | grep -q 'secretName: dex-ca' || fail "the CA Secret is not mounted"
deployment "${DEX[@]}" | grep -A3 'name: DEX_CLIENT_SECRET' | grep -q 'name: idp$' || fail "the client secret does not come from global.identity.existingSecret"
if render "${DEX[@]}" --show-only templates/oauth-secret.yaml 2>/dev/null | grep -q 'kind: Secret'; then
  fail "an existing identity Secret still renders the chart's own"
fi

# OAuth off renders no OAuth flag and no CA mount.
got=$(args)
if echo "$got" | grep -q -- '--enable-oauth\|--dex-\|--oauth-'; then fail "oauth disabled renders OAuth flags: '$got'"; fi

# The MCPServer: the tool-group label the platform tiers by, the in-cluster
# URL, the auth block with the audiences muster requests at login.
mcp=$(render "${DEX[@]}" --set muster.mcpServer.enabled=true --set-json 'muster.mcpServer.auth.requiredAudiences=["kubernetes"]' --show-only templates/mcpserver.yaml)
echo "$mcp" | grep -q '^ *agent-platform.giantswarm.io/tool-group: agent-platform$' || fail "the MCPServer lacks the agent-platform tool-group label"
echo "$mcp" | grep -q '^ *muster.giantswarm.io/type: vm-manager$' || fail "the MCPServer lacks the muster type label"
echo "$mcp" | grep -q 'url: http://vmm-vm-manager.default.svc.cluster.local:8080/mcp' || fail "the MCPServer URL is not the in-cluster Service"
echo "$mcp" | grep -q 'forwardToken: true' || fail "the MCPServer does not forward the token"
echo "$mcp" | grep -q -- '^ *- kubernetes$' || fail "the MCPServer does not list requiredAudiences"
if render --set muster.mcpServer.enabled=true --show-only templates/mcpserver.yaml | grep -q 'auth:'; then
  fail "the MCPServer carries an auth block without oauth.enabled"
fi
if render --show-only templates/mcpserver.yaml 2>/dev/null | grep -q 'kind: MCPServer'; then
  fail "the MCPServer renders while muster.mcpServer.enabled is false"
fi

# The guests' egress: the chart's own policy admits every destination unless
# guestEgress is off.
render --set networkPolicy.enabled=true --show-only templates/networkpolicy.yaml | grep -q '^ *- {}$' || fail "guestEgress on: the policy must admit every egress destination"
if render --set networkPolicy.enabled=true --set networkPolicy.guestEgress=false --show-only templates/networkpolicy.yaml | grep -q '^ *- {}$'; then
  fail "guestEgress off still admits every destination"
fi

# The schema refuses a key the chart does not know.
if helm template vmm "$CHART" --set bogus=1 >/dev/null 2>&1; then
  fail "values.schema.json accepted an unknown key"
fi

echo "verify-chart: ok"
