package models

type CSIMountConfig struct {
	MountID   string `json:"mountID"`   // mount id
	PvName    string `json:"pvName"`    // persistent volume name for mounting
	MountPath string `json:"mountPath"` // mount target in container to mount the persistent volume
	SubPath   string `json:"subPath"`   // sub path address in persistent volume
	ReadOnly  bool   `json:"readOnly"`  // whether to mount the persistent volume as read-only
}
