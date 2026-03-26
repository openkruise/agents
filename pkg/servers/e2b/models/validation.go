package models

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/openkruise/agents/api/v1alpha1"
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

// parseAndValidatePersistentContents parses and validates persistent contents string.
// Valid values are: "memory", "filesystem", or "memory,filesystem" (order doesn't matter).
// Duplicates are not allowed.
func parseAndValidatePersistentContents(contents string) ([]string, error) {
	if contents == "" {
		return nil, nil
	}

	parts := strings.Split(contents, ",")
	result := make(map[string]struct{})

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Validate each value
		if part != v1alpha1.CheckpointPersistentContentMemory && part != v1alpha1.CheckpointPersistentContentFilesystem {
			return nil, fmt.Errorf("invalid persistent content %q, only %q and %q are allowed",
				part, v1alpha1.CheckpointPersistentContentMemory, v1alpha1.CheckpointPersistentContentFilesystem)
		}

		result[part] = struct{}{}
	}

	// convert result to slice
	resultSlice := make([]string, 0, len(result))
	for part := range result {
		resultSlice = append(resultSlice, part)
	}
	return resultSlice, nil
}
