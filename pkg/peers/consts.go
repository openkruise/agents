package peers

const (
	NodePrefixSandboxManager = "sm-"
	NodePrefixSandboxGateway = "sg-"
	// NodePrefixSandboxController marks sender-only members. The agent sandbox
	// controller publishes route refreshes but never serves them, so GetPeers
	// excludes these members from every refresh target list.
	NodePrefixSandboxController = "sc-"
)
