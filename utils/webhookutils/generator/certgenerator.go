package generator

// Artifacts hosts a private key, its corresponding serving certificate and
// the CA certificate that signs the serving certificate.
type Artifacts struct {
	// PEM encoded private key
	Key []byte
	// PEM encoded serving certificate
	Cert []byte
	// PEM encoded CA private key
	CAKey []byte
	// PEM encoded CA certificate
	CACert []byte
	// Resource version of the certs
	ResourceVersion string
}

// CertGenerator is an interface to provision the serving certificate.
type CertGenerator interface {
	// Generate returns a Artifacts struct.
	Generate(CommonName string) (*Artifacts, error)
	// SetCA sets the PEM-encoded CA private key and CA cert for signing the generated serving cert.
	SetCA(caKey, caCert []byte)
}
