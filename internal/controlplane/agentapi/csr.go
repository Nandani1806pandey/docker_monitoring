package agentapi

import "encoding/pem"

// decodeCSRPEM extracts the DER bytes from a PEM-encoded CSR, or returns
// nil if the input isn't valid PEM.
func decodeCSRPEM(csrPEM []byte) []byte {
	block, _ := pem.Decode(csrPEM)
	if block == nil {
		return nil
	}
	return block.Bytes
}
