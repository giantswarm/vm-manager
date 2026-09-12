package imds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/giantswarm/vm-manager/internal/apierr"
)

// CredentialsKey is the JSON member systemd-imds --import looks for in
// user-data: an array of credentials it writes into /run/credstore/.
const CredentialsKey = "systemd.credentials" //nolint:gosec // G101: a JSON member name, not a secret

// Credential is one entry of the systemd.credentials envelope. Exactly one of
// Text and Data is set; Data is base64 on the wire.
type Credential struct {
	Name string `json:"name"`
	Text string `json:"text,omitempty"`
	Data []byte `json:"data,omitempty"`
}

// UserDataCredentialsWrapper wraps payload as the user-data envelope
//
//	{"systemd.credentials":[{"name":name,"text":...|"data":...}]}
//
// so systemd-imds --import in the initrd stores it as credential name for
// systemd units to consume (ImportCredential=, LoadCredential=). Use it when
// the guest should receive user-data as a credential, e.g. a firstboot or
// sysinstall configuration. The default /user-data body is the raw Ignition
// JSON because Ignition fetches and parses it itself and would not understand
// the envelope. Text is used when payload is valid UTF-8 without NUL bytes,
// Data otherwise.
func UserDataCredentialsWrapper(name string, payload []byte) ([]byte, error) {
	if err := validCredentialName(name); err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: credential %q has no payload", apierr.ErrInvalid, name)
	}
	c := Credential{Name: name}
	if utf8.Valid(payload) && !bytes.ContainsRune(payload, 0) {
		c.Text = string(payload)
	} else {
		c.Data = payload
	}
	return json.Marshal(map[string][]Credential{CredentialsKey: {c}})
}

// validCredentialName mirrors systemd's credential_name_valid: a plain file
// name of printable ASCII without '/', ':' or whitespace, at most 255 bytes.
func validCredentialName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return fmt.Errorf("%w: invalid credential name %q", apierr.ErrInvalid, name)
	}
	for i := range len(name) {
		if c := name[i]; c <= ' ' || c >= 0x7f || c == '/' || c == ':' {
			return fmt.Errorf("%w: invalid credential name %q", apierr.ErrInvalid, name)
		}
	}
	return nil
}
