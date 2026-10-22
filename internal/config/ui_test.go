package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsoleRequiresGatewayAuthentication(t *testing.T) {
	for _, key := range []string{"", "   ", "gateway-secret"} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "router.yaml")
			body := "ui:\n  enabled: true\napiKey: \"" + key + "\"\nupstreams:\n  api:\n    baseUrl: https://example.invalid/v1\nmodels:\n  chat:\n    api: openai-completions\n    targets:\n      - upstream: api\n        model: chat\n        weight: 1\n"
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if strings.TrimSpace(key) == "" {
				if err == nil || !strings.Contains(err.Error(), "ui.enabled requires") {
					t.Fatalf("unauthenticated console config accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
