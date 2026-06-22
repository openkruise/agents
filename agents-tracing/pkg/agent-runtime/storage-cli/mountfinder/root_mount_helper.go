package mountfinder

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

type MountReader interface {
	ReadMounts() ([]string, error)
}

type SystemMountReader struct{}

// procMountsPath is the path SystemMountReader.ReadMounts opens. It is a
// package variable so that unit tests can point it at a fixture file on
// systems that do not expose /proc/mounts (e.g. macOS). Production code
// MUST NOT reassign it.
var procMountsPath = "/proc/mounts"

func (r *SystemMountReader) ReadMounts() ([]string, error) {
	file, err := os.Open(procMountsPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

// newSystemMountReader is the indirection used by FoundValidMountPath to
// obtain a MountReader. Tests in the same package may substitute it to
// inject a mock; production code MUST NOT reassign it.
var newSystemMountReader = func() MountReader {
	return &SystemMountReader{}
}

// execMountCommand is the indirection used by findMountPathWithMountCmd to
// obtain the raw `mount` command output. Tests in the same package may
// substitute it to inject canned output; production code MUST NOT
// reassign it.
var execMountCommand = func() ([]byte, error) {
	return exec.Command("mount").Output()
}

func findMountPathByNameWithReader(reader MountReader, mountName string) (string, error) {
	lines, err := reader.ReadMounts()
	if err != nil {
		return "", err
	}

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			mountPoint := fields[1]
			if strings.Contains(mountPoint, mountName) {
				return mountPoint, nil
			}
		}
	}

	return "", fmt.Errorf("mount point containing '%s' not found", mountName)
}

func findMountPathWithMountCmd(mountName string) (string, error) {
	output, err := execMountCommand()
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, mountName) {
			// Parse the mount point. Typical format:
			// /dev/sda1 on /path type ext4 (rw,relatime)
			parts := strings.Split(line, " on ")
			if len(parts) >= 2 {
				mountPoint := strings.Split(parts[1], " ")[0]
				return mountPoint, nil
			}
		}
	}

	return "", fmt.Errorf("mount point '%s' not found", mountName)
}

func FindMountPath(mountName string, debug bool) (string, error) {
	reader := newSystemMountReader()
	mountPath, err := findMountPathByNameWithReader(reader, mountName)
	if err != nil {
		log.Printf("Error finding mount path via /proc/mounts: %v\n", err)
		// Fall back to the `mount` command.
		mountPath, err = findMountPathWithMountCmd(mountName)
		if err != nil {
			log.Printf("Error finding mount path with mount command: %v\n", err)
			return "", err
		}
	}

	if !checkMountPathExists(mountPath) {
		return "", fmt.Errorf("mount path %s is not accessible", mountPath)
	}
	if debug {
		log.Printf("[DEBUG] Mount path %s exists and is accessible\n", mountPath)
	}
	return mountPath, nil
}

func checkMountPathExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
