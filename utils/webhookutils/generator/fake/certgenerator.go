package fake

import (
	"bytes"
	"fmt"

	"gitlab.alibaba-inc.com/serverlessinfra/agents/utils/webhookutils/generator"
)

// CertGenerator is a certGenerator for testing.
type CertGenerator struct {
	CAKey                  []byte
	CACert                 []byte
	DNSNameToCertArtifacts map[string]*generator.Artifacts
}

var _ generator.CertGenerator = &CertGenerator{}

// SetCA sets the PEM-encoded CA private key and CA cert for signing the generated serving cert.
func (cp *CertGenerator) SetCA(CAKey, CACert []byte) {
	cp.CAKey = CAKey
	cp.CACert = CACert
}

// Generate generates certificates by matching a common name.
func (cp *CertGenerator) Generate(commonName string) (*generator.Artifacts, error) {
	certs, found := cp.DNSNameToCertArtifacts[commonName]
	if !found {
		return nil, fmt.Errorf("failed to find common name %q in the certGenerator", commonName)
	}
	if cp.CAKey != nil && cp.CACert != nil &&
		!bytes.Contains(cp.CAKey, []byte("invalid")) && !bytes.Contains(cp.CACert, []byte("invalid")) {
		certs.CAKey = cp.CAKey
		certs.CACert = cp.CACert
	}
	return certs, nil
}
