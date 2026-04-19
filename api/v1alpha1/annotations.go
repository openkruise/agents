package v1alpha1

// common SandboxSet annotations

const (
	AnnotationRuntimeURL         = InternalPrefix + "runtime-url"
	AnnotationRuntimeAccessToken = InternalPrefix + "runtime-access-token"
)

// E2B annotations

const (
	E2BPrefix      = "e2b." + InternalPrefix
	E2BLabelPrefix = "label:"

	AnnotationEnvdAccessToken = E2BPrefix + "envd-access-token"
	AnnotationEnvdURL         = E2BPrefix + "envd-url"
)

// Upgrade annotations

const (
	// AnnotationReserveInstance is the annotation key to control whether to reserve the underlying instance.
	AnnotationReserveInstance = "ops.alibabacloud.com/reserve-instance"

	// AnnotationUpgradeState is the annotation key to track the upgrade sub-phase of a sandbox.
	AnnotationUpgradeState = InternalPrefix + "upgrade-state"
)

const True = "true"
const False = "false"
