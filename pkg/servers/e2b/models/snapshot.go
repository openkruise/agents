package models

type Snapshot struct {
	SnapshotID string   `json:"snapshotID"`
	Names      []string `json:"names"`
}
