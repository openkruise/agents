package v1alpha1

// common SandboxSet annotations

const (
	AnnotationRuntimeURL         = InternalPrefix + "runtime-url"
	AnnotationRuntimeAccessToken = InternalPrefix + "runtime-access-token"
)

// E2B annotations

const (
	E2BPrefix = "e2b." + InternalPrefix

	AnnotationEnvdAccessToken = E2BPrefix + "envd-access-token"
	AnnotationEnvdURL         = E2BPrefix + "envd-url"
)

const True = "true"
