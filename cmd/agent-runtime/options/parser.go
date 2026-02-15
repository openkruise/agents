package options

import (
	"flag"
	"os"
	"strconv"
)

func InitFlagOptions() {
	// Set default values
	if portEnv, err := strconv.Atoi(os.Getenv("SERVER_PORT")); err == nil {
		ServerPort = portEnv
	} else {
		ServerPort = defaultHttpServerPort
	}

	if logLevelEnv, err := strconv.Atoi(os.Getenv("LOG_LEVEL")); err == nil {
		ServerLogLevel = logLevelEnv
	} else {
		ServerLogLevel = defaultLogLevel
	}

	flag.IntVar(&ServerPort, "port", ServerPort, "SandboxRuntime Http Server listening port (default: 9527)")
	flag.IntVar(&ServerLogLevel, "log-level", ServerLogLevel,
		"Server log level — Specifies the logging verbosity as an integer from 0 to 7, where 0=LevelEmergency, 1=LevelAlert, 2=LevelCritical, 3=LevelError, 4=LevelWarning, 5=LevelNotice, 6=LevelInformational, and 7=LevelDebug; defaults to 6 (LevelInformational).)")
	flag.String("workspace", "",
		"Root directory for file operations — Specifies the base path used for file system operations; defaults to the current working directory if not provided.\n\n")
	flag.BoolVar(&EnablePprof, "enable-pprof", false, "Enable pprof profiling")
	flag.StringVar(&PprofAddr, "pprof-addr", ":6060", "The address the pprof debug maps to.")

	flag.StringVar(&ValidTokens, "valid-tokens", "", "The valid tokens for authentication")
	flag.StringVar(&AllowedPaths, "allowed-paths", "", "The allowed paths for authentication")
	flag.BoolVar(&EnableSigning, "enable-signing", true, "Enable signing for authentication")

	// flag config
	flag.BoolVar(&VersionFlag, "version", false, "To print envd version")
	flag.StringVar(&StartCmdFlag, "cmd", "", "A command to run on the daemon start")

	flag.BoolVar(&IsNotFC, "isnotfc", false, "isNotFCmode prints all logs to stdout")

	// Parse flags - these will override environment variables if provided
	flag.Parse()
}
