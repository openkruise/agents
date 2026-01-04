package v1alpha1

// common SandboxSet annotations

const (
	// AnnotationReserveFailedSandbox is used to declare whether the Sandbox in the SandboxSet should be reserved.
	// when an error occurs during the claiming process (such as dynamic volume mounting failure,
	// in-place upgrade failure, configuration change failure, etc.).
	// The default value is false, which will directly delete all failed Sandboxes.
	AnnotationReserveFailedSandbox = InternalPrefix + "reserve-failed-sandbox"
)

// E2B annotations

const (
	E2BPrefix = "e2b." + InternalPrefix

	// AnnotationShouldInitEnvd is used to declare that when a Sandbox in a SandboxSet is Claimed,
	// an envd initialization operation needs to be performed.
	AnnotationShouldInitEnvd  = E2BPrefix + "should-init-envd"
	AnnotationEnvdAccessToken = E2BPrefix + "envd-access-token"
	AnnotationEnvdURL         = E2BPrefix + "envd-url"
)

const True = "true"
