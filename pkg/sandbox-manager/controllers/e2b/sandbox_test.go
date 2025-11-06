package e2b

import (
	"testing"
)

func TestReplacer(t *testing.T) {
	url := "ws://localhost:9222/devtools/browser/12345678-1234-1234-1234-123456789012"
	url = browserWebSocketReplacer.ReplaceAllString(url, "ws://hello-world")
	if url != "ws://hello-world/devtools/browser/12345678-1234-1234-1234-123456789012" {
		t.Errorf("Expected %s, got %s", "ws://hello-world/devtools/browser/12345678-1234-1234-1234-123456789012", url)
	}
}
