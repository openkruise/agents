package v1alpha1

type CSIMountConfig struct {
	MountID   string `json:"mountID,omitempty"`  // mount id
	PvName    string `json:"pvName"`             // persistent volume name for mounting
	MountPath string `json:"mountPath"`          // mount target in container to mount the persistent volume
	SubPath   string `json:"subPath,omitempty"`  // sub path address in persistent volume
	ReadOnly  bool   `json:"readOnly,omitempty"` // whether to mount the persistent volume as read-only
}
