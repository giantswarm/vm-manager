package imds

import (
	"fmt"
	"net/http"
	"strings"
)

// Wire constants shared by the hwdb record, the sysupdate transfers in the
// image and the router.
const (
	// Vendor is the IMDS_VENDOR the guest reports for VMs run by vm-manager.
	Vendor = "giantswarm"
	// Address is the link-local address the guest reaches the service at.
	Address = "169.254.169.254"
	// BasePath is the URL prefix every key lives under.
	BasePath = "/giantswarm/v1"
	// DataURL is IMDS_DATA_URL: what systemd-imdsd prepends to a key.
	DataURL = "http://" + Address + BasePath
	// HwdbMatch is the DMI modalias glob of the provider record; vm-manager
	// starts QEMU with -smbios type=1,manufacturer=GiantSwarm,product=vm-manager.
	HwdbMatch = "dmi:*:svnGiantSwarm:*"
	// HwdbFile is where the image build picks up HwdbRecord, relative to the
	// repository root.
	HwdbFile = "images/mkosi.images/base/mkosi.extra/usr/lib/udev/hwdb.d/45-imds-giantswarm.hwdb"
)

// Key is one entry of the IMDS contract: the path below DataURL, the method it
// answers to and, for the keys systemd-imdsd knows by role, the hwdb property
// that points the guest at it.
type Key struct {
	// Path below DataURL, beginning with "/". For subtrees this is the
	// representative path the hwdb or the guest tooling uses.
	Path string
	// Pattern is the http.ServeMux pattern when it differs from Path.
	Pattern string
	// Method is the HTTP method the key answers to.
	Method string
	// Property is the IMDS_KEY_* hwdb property naming this key; empty for
	// keys only vm-manager's own guest tooling requests.
	Property string
	// Description says what the guest does with the key.
	Description string
}

// pattern is the ServeMux pattern relative to BasePath.
func (k Key) pattern() string {
	if k.Pattern != "" {
		return k.Pattern
	}
	return k.Path
}

// Keys is the IMDS contract: it generates the hwdb record (HwdbRecord) and the
// router (Handler). The hwdb-backed keys come first, in the order the record
// lists them.
var Keys = []Key{
	{Path: "/hostname", Method: http.MethodGet, Property: "IMDS_KEY_HOSTNAME",
		Description: "VM host name; systemd-imds --import sets it as the firstboot.hostname credential"},
	{Path: "/region", Method: http.MethodGet, Property: "IMDS_KEY_REGION",
		Description: "host name the VM runs on"},
	{Path: "/zone", Method: http.MethodGet, Property: "IMDS_KEY_ZONE",
		Description: "network name the VM is attached to"},
	{Path: "/public-keys/0", Pattern: "/public-keys/{index}", Method: http.MethodGet, Property: "IMDS_KEY_SSH_KEY",
		Description: "n-th authorized key; 0 is imported as ssh.authorized_keys.root"},
	{Path: "/user-data", Method: http.MethodGet, Property: "IMDS_KEY_USERDATA",
		Description: "CAPI bootstrap data as Ignition JSON, gated by the initrd-stage attestation"},
	{Path: "/instance-id", Method: http.MethodGet,
		Description: "vm-manager's VM ID"},
	{Path: "/kubernetes-version", Method: http.MethodGet,
		Description: "version the guest passes to systemd-sysupdate --component=kubernetes update"},
	{Path: "/public-keys/", Pattern: "/public-keys/{$}", Method: http.MethodGet,
		Description: "index of authorized keys, one line per key: 0, 1, ..."},
	{Path: "/metadata/", Pattern: "/metadata/{key}", Method: http.MethodGet,
		Description: "free-form per-VM values"},
	{Path: "/attest/nonce", Method: http.MethodGet,
		Description: "fresh nonce for the TPM quote, valid NonceTTL"},
	{Path: "/attest/quote", Method: http.MethodPost,
		Description: "QuoteRequest JSON in, QuoteResult JSON out; a verified initrd-stage quote releases /user-data"},
	{Path: "/report", Method: http.MethodPost,
		Description: "systemd-report upload --url= sink; JSON in, 204 out"},
	{Path: "/sysupdate/", Pattern: "/sysupdate/{component}/{file}", Method: http.MethodGet,
		Description: "SHA256SUMS, SHA256SUMS.gpg and artifacts of a sysupdate component, served unfiltered"},
}

const hwdbHeader = `# systemd-imdsd provider record for VMs run by vm-manager.
# Matched against /sys/class/dmi/id/modalias; vm-manager starts QEMU with
# -smbios type=1,manufacturer=GiantSwarm,product=vm-manager.
# Keys follow the IMDS contract in docs/design.md (the Go key table is the
# source of truth; keep this file in sync).

`

// HwdbRecord renders the provider record the image installs as HwdbFile.
// Change Keys, regenerate the file, and TestHwdbRecordParity keeps them equal.
func HwdbRecord() string {
	var b strings.Builder
	b.WriteString(hwdbHeader)
	fmt.Fprintf(&b, "%s\n IMDS_VENDOR=%s\n IMDS_DATA_URL=%s\n IMDS_ADDRESS_IPV4=%s\n", HwdbMatch, Vendor, DataURL, Address)
	for _, k := range Keys {
		if k.Property != "" {
			fmt.Fprintf(&b, " %s=%s\n", k.Property, k.Path)
		}
	}
	return b.String()
}
