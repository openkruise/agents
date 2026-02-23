package models

import (
	"fmt"
	"path/filepath"
	"strings"
)

func validateMountPoint(mountPoint string) error {
	if mountPoint == "" {
		return fmt.Errorf("mount point cannot be empty")
	}

	// to start with / path
	if !strings.HasPrefix(mountPoint, "/") {
		return fmt.Errorf("mount point must start with '/'")
	}

	// to check for any occurrence of .. in the path
	if strings.Contains(mountPoint, "..") {
		return fmt.Errorf("mount point contains invalid '..' path element")
	}

	// to parse the path, eliminating relative path symbols such as "." and ".."
	cleanPath := filepath.Clean(mountPoint)
	if cleanPath != mountPoint {
		return fmt.Errorf("mount point contains invalid path elements like '..' or '.'")
	}

	return nil
}
