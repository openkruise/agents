package filesystemconnect

// These constants are the fully-qualified names of the RPCs defined in this package. They're
// exposed at runtime as Spec.Procedure and as the final two segments of the HTTP route.
//
// Note that these are different from the fully-qualified method names used by
// google.golang.org/protobuf/reflect/protoreflect. To convert from these constants to
// reflection-formatted method names, remove the leading slash and convert the remaining slash to a
// period.
const (
	// ProcessListProcedure is the fully-qualified name of the Process's List RPC.
	ProcessListProcedure = "/process.Process/List"
	// ProcessConnectProcedure is the fully-qualified name of the Process's Connect RPC.
	ProcessConnectProcedure = "/process.Process/Connect"
	// ProcessStartProcedure is the fully-qualified name of the Process's Start RPC.
	ProcessStartProcedure = "/process.Process/Start"
	// ProcessUpdateProcedure is the fully-qualified name of the Process's Update RPC.
	ProcessUpdateProcedure = "/process.Process/Update"
	// ProcessStreamInputProcedure is the fully-qualified name of the Process's StreamInput RPC.
	ProcessStreamInputProcedure = "/process.Process/StreamInput"
	// ProcessSendInputProcedure is the fully-qualified name of the Process's SendInput RPC.
	ProcessSendInputProcedure = "/process.Process/SendInput"
	// ProcessSendSignalProcedure is the fully-qualified name of the Process's SendSignal RPC.
	ProcessSendSignalProcedure = "/process.Process/SendSignal"
)

// These constants are the fully-qualified names of the RPCs defined in this package. They're
// exposed at runtime as Spec.Procedure and as the final two segments of the HTTP route.
//
// Note that these are different from the fully-qualified method names used by
// google.golang.org/protobuf/reflect/protoreflect. To convert from these constants to
// reflection-formatted method names, remove the leading slash and convert the remaining slash to a
// period.
const (
	// FilesystemStatProcedure is the fully-qualified name of the Filesystem's Stat RPC.
	FilesystemStatProcedure = "/filesystem.Filesystem/Stat"
	// FilesystemMakeDirProcedure is the fully-qualified name of the Filesystem's MakeDir RPC.
	FilesystemMakeDirProcedure = "/filesystem.Filesystem/MakeDir"
	// FilesystemMoveProcedure is the fully-qualified name of the Filesystem's Move RPC.
	FilesystemMoveProcedure = "/filesystem.Filesystem/Move"
	// FilesystemListDirProcedure is the fully-qualified name of the Filesystem's ListDir RPC.
	FilesystemListDirProcedure = "/filesystem.Filesystem/ListDir"
	// FilesystemRemoveProcedure is the fully-qualified name of the Filesystem's Remove RPC.
	FilesystemRemoveProcedure = "/filesystem.Filesystem/Remove"
	// FilesystemWatchDirProcedure is the fully-qualified name of the Filesystem's WatchDir RPC.
	FilesystemWatchDirProcedure = "/filesystem.Filesystem/WatchDir"
	// FilesystemCreateWatcherProcedure is the fully-qualified name of the Filesystem's CreateWatcher
	// RPC.
	FilesystemCreateWatcherProcedure = "/filesystem.Filesystem/CreateWatcher"
	// FilesystemGetWatcherEventsProcedure is the fully-qualified name of the Filesystem's
	// GetWatcherEvents RPC.
	FilesystemGetWatcherEventsProcedure = "/filesystem.Filesystem/GetWatcherEvents"
	// FilesystemRemoveWatcherProcedure is the fully-qualified name of the Filesystem's RemoveWatcher
	// RPC.
	FilesystemRemoveWatcherProcedure = "/filesystem.Filesystem/RemoveWatcher"
)
