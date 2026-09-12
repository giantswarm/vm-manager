package quote

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
)

// PCRCount is the PC Client minimum allocation; the pcrs command prints all
// of them.
const PCRCount = 24

// AllPCRs returns 0..PCRCount-1.
func AllPCRs() []uint {
	pcrs := make([]uint, PCRCount)
	for i := range pcrs {
		pcrs[i] = uint(i) // #nosec G115 -- i < PCRCount
	}
	return pcrs
}

// ReadPCRs returns the values of pcrs in bank alg. A TPM answers at most a
// few PCRs per TPM2_PCR_Read, so the selection is read in rounds until every
// requested index has a value.
func ReadPCRs(tpm transport.TPM, alg tpm2.TPMAlgID, pcrs []uint) (map[uint][]byte, error) {
	values := make(map[uint][]byte, len(pcrs))
	remaining := append([]uint(nil), pcrs...)
	for len(remaining) > 0 {
		rsp, err := tpm2.PCRRead{PCRSelectionIn: selection(alg, remaining)}.Execute(tpm)
		if err != nil {
			return nil, err
		}
		got := selected(rsp.PCRSelectionOut, alg)
		if len(got) == 0 || len(got) != len(rsp.PCRValues.Digests) {
			return nil, fmt.Errorf("pcr read of %v returned %d selections and %d digests", remaining, len(got), len(rsp.PCRValues.Digests))
		}
		for i, idx := range got {
			values[idx] = rsp.PCRValues.Digests[i].Buffer
		}
		remaining = without(remaining, values)
	}
	return values, nil
}

// selection builds the TPML_PCR_SELECTION for one bank.
func selection(alg tpm2.TPMAlgID, pcrs []uint) tpm2.TPMLPCRSelection {
	return tpm2.TPMLPCRSelection{PCRSelections: []tpm2.TPMSPCRSelection{{
		Hash:      alg,
		PCRSelect: tpm2.PCClientCompatible.PCRs(pcrs...),
	}}}
}

// selected lists the indexes set in sel's bank alg, ascending, which is the
// order the TPM returns digests in.
func selected(sel tpm2.TPMLPCRSelection, alg tpm2.TPMAlgID) []uint {
	var out []uint
	for _, s := range sel.PCRSelections {
		if s.Hash != alg {
			continue
		}
		for byteIdx, b := range s.PCRSelect {
			for bit := range 8 {
				if b&(1<<bit) != 0 {
					out = append(out, uint(byteIdx*8+bit)) // #nosec G115 -- byteIdx and bit are small non-negative
				}
			}
		}
	}
	return out
}

// without drops the indexes already in have; it errors out through the
// caller's zero-progress check if the TPM answers nothing new.
func without(pcrs []uint, have map[uint][]byte) []uint {
	var rest []uint
	for _, p := range pcrs {
		if _, ok := have[p]; !ok {
			rest = append(rest, p)
		}
	}
	return rest
}

// hexPCRs renders values as the index -> hex map of imds.QuoteRequest.PCRs.
func hexPCRs(values map[uint][]byte) map[string]string {
	out := make(map[string]string, len(values))
	for idx, v := range values {
		out[strconv.FormatUint(uint64(idx), 10)] = hex.EncodeToString(v)
	}
	return out
}

// DecodeNonce turns the hex nonce from /attest/nonce into the qualifying
// data bytes of the quote.
func DecodeNonce(nonce string) ([]byte, error) {
	if nonce == "" {
		return nil, errors.New("empty nonce")
	}
	raw, err := hex.DecodeString(nonce)
	if err != nil {
		return nil, fmt.Errorf("nonce is not hex: %w", err)
	}
	return raw, nil
}

// BankAlg maps a bank name as used in imds.QuoteRequest.PCRs to its TPM
// algorithm.
func BankAlg(bank string) (tpm2.TPMAlgID, error) {
	switch bank {
	case "sha1":
		return tpm2.TPMAlgSHA1, nil
	case "sha256":
		return tpm2.TPMAlgSHA256, nil
	case "sha384":
		return tpm2.TPMAlgSHA384, nil
	case "sha512":
		return tpm2.TPMAlgSHA512, nil
	default:
		return 0, fmt.Errorf("unknown pcr bank %q", bank)
	}
}
