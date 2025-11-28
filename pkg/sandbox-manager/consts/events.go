package consts

type EventType string

const (
	SandboxCreated = EventType("SandboxCreated")
	SandboxReady   = EventType("SandboxReady")
	SandboxKill    = EventType("SandboxKill")
)
