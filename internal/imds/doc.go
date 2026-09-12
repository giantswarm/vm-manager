// Package imds implements the Giant Swarm instance metadata service: the HTTP
// side of the contract systemd-imdsd in the guest is configured for by the
// hwdb record the image ships (HwdbFile, generated from Keys by HwdbRecord).
//
// # Contract
//
// The guest resolves a key as plain HTTP GET DataURL + key; every body is
// text/plain without a trailing newline unless the value carries one. 404 is
// what systemd-imdsd maps to KeyNotFound, 405 a wrong method; /user-data has
// its own codes (below) because its client is Ignition, not systemd-imdsd.
// No token flow is configured, so
// there is no IMDS_TOKEN_URL and no header the guest has to send. The client
// address on the in-stack listener is the only identity: a request from an
// address the Resolver does not map to a VM is answered 403 for every key,
// and X-Forwarded-For is never consulted.
//
//	Key                            Method  Guest side
//	/hostname                      GET     systemd-imds --import -> firstboot.hostname
//	/region, /zone                 GET     systemd-imds -K region|zone (host name, network name)
//	/public-keys/                  GET     index, one line per key: 0, 1, ...
//	/public-keys/<n>               GET     <n>=0 is IMDS_KEY_SSH_KEY -> ssh.authorized_keys.root
//	/user-data                     GET     CAPI bootstrap data (Ignition JSON), gated, see below
//	/instance-id                   GET     vm-manager's VM ID
//	/kubernetes-version            GET     version to pass to systemd-sysupdate, see below
//	/metadata/<k>                  GET     free-form per-VM values
//	/attest/nonce                  GET     fresh nonce for the TPM quote
//	/attest/quote                  POST    QuoteRequest JSON -> QuoteResult JSON
//	/report                        POST    systemd-report upload --url= sink
//	/sysupdate/<component>/<file>  GET     SHA256SUMS, SHA256SUMS.gpg and artifacts
//
// # Attestation gating
//
// /user-data is served only once Instance.UserDataReleased is true. The
// Attestor decides whether a quote verifies; ReleasesUserData is the one rule
// that turns a verified quote into a release: only the initrd stage unlocks
// user-data, the ready stage is recorded for get_vm_attestation. Until then
// /user-data answers 503 with Retry-After and a one-line reason, which
// Ignition (the only client of this key; it is not an hwdb key) retries with
// backoff until its fetch timeout; a VM without user-data gets 204, which
// Ignition takes as "no config" and finishes its stages with nothing to do.
// 404 and 403 would both fail Ignition's fetch stage, see userData.
//
// # Version selection for sysupdate
//
// The sysupdate directories are served unfiltered: their SHA256SUMS is signed
// at image build time and cannot be rewritten per VM, so a directory lists
// every version the host has. The guest selects its version explicitly by
// reading the plain key /kubernetes-version and running
//
//	systemd-sysupdate --component=kubernetes update <version>
//
// The file names must match the MatchPattern of the transfers in the image
// (giantswarm-vm-base_@v_@u.root.raw etc. under /sysupdate/base/,
// kubernetes_@v.raw under /sysupdate/kubernetes/); the ArtifactSource is only
// asked for what the guest requests, directory listings are never produced.
//
// # Keeping the image in sync
//
// Keys is the single source of truth. Changing a key that carries an hwdb
// property means regenerating HwdbFile from HwdbRecord; TestHwdbRecordParity
// fails until the file in images/ matches.
package imds
