package utils

import (
	"fmt"
)

func GetSandboxAddress(sandboxId, domain string, port int32) string {
	return fmt.Sprintf("%d-%s.%s", port, sandboxId, domain)
}
