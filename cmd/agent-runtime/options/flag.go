package options

const (
	defaultHttpServerPort = 49983
	defaultLogLevel       = 6
)

var (
	// ServerPort defines the network port that the HTTP server binds to.
	ServerPort int

	// ServerLogLevel controls the logging level of the server.
	ServerLogLevel int

	Workspace string

	// ValidTokens defines the valid token for authentication.
	ValidTokens string
	// AllowedPaths defines the allowed paths for skip authentication.
	AllowedPaths  string
	// EnableSigning defines whether to enable signature validation.
	EnableSigning bool

	// Enable pprof config for debug
	EnablePprof bool
	PprofAddr   string

	// Some flags config for agent-runtime
	VersionFlag  bool
	StartCmdFlag string

	IsNotFC bool
)
