package config_test

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llm-router/internal/config"
)

func FuzzConfigDecode(f *testing.F) {
	for _, seed := range []string{"", "[]", "scalar", "listen: x\nlisten: y", "defaults:\n  requestTimeout: nonsense", "upstreams: {}\nmodels: {}", "models:\n  missing:\n    targets: [{upstream: absent, weight: 1}]"} {
		f.Add([]byte(seed), uint8(0))
	}
	f.Add([]byte("# compatibility"), uint8(1))
	f.Add([]byte("# reordered"), uint8(2))
	f.Fuzz(func(t *testing.T, raw []byte, mode uint8) {
		if len(raw) > 64<<10 {
			return
		} // Explicit bounded document search domain.
		dir := t.TempDir()
		path := filepath.Join(dir, "router.yaml")
		load := func(body []byte) (*config.Config, error) {
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			return config.LoadConfig(path)
		}
		if mode%3 != 0 {
			// Hex keeps arbitrary input a safe nonblank YAML scalar without a second decoder.
			data := raw
			if len(data) > 128 {
				data = data[:128]
			}
			suffix := hex.EncodeToString(data)
			aliasName, modelName, upstreamName := "alias-"+suffix, "model-"+suffix, "upstream-"+suffix
			weight := 1 + int(mode)
			upstream := fmt.Sprintf("upstreams:\n  %q:\n    baseUrl: http://127.0.0.1:1\n", upstreamName)
			models := fmt.Sprintf("models:\n  %q:\n    api: openai-completions\n    targets:\n      - upstream: %q\n        model: %q\n        weight: %d\n", aliasName, upstreamName, modelName, weight)
			assertFields := func(cfg *config.Config) {
				t.Helper()
				if cfg == nil || len(cfg.Models) != 1 || len(cfg.Upstreams) != 1 {
					t.Fatal("valid generated config lost alias/upstream")
				}
				upstream, ok := cfg.Upstreams[upstreamName]
				if !ok || upstream.BaseURL != "http://127.0.0.1:1" {
					t.Fatal("generated upstream changed")
				}
				alias, ok := cfg.Models[aliasName]
				if !ok || alias.API != config.APIOpenAICompletions || len(alias.Targets) != 1 {
					t.Fatal("generated alias changed")
				}
				target := alias.Targets[0]
				if target.Upstream != upstreamName || target.Model != modelName || target.Weight != weight {
					t.Fatalf("generated target changed: %+v", target)
				}
			}
			baseline, err := load([]byte(upstream + models))
			if err != nil {
				t.Fatalf("valid fixture: %v", err)
			}
			assertFields(baseline)
			cosmetic := "# harmless generated comment " + suffix + "\n" + models + upstream
			if mode%2 == 0 {
				cosmetic = "# alternate order " + suffix + "\n" + upstream + models
			}
			got, err := load([]byte(cosmetic))
			if err != nil {
				t.Fatalf("cosmetic fixture: %v", err)
			}
			assertFields(got)
			if _, err := load([]byte(cosmetic + "guaranteed_unknown_fuzz_schema_key: true\n")); err == nil {
				t.Fatal("unknown top-level schema key accepted")
			}
			return
		}
		cfg, err := load(raw)
		if err != nil {
			return
		}
		if cfg == nil || len(cfg.Upstreams) == 0 || len(cfg.Models) == 0 {
			t.Fatal("accepted config has no upstreams or aliases")
		}
		if cfg.UI.Enabled && strings.TrimSpace(cfg.APIKey) == "" {
			t.Fatal("enabled UI accepted without gateway key")
		}
		known := func(p config.APIProtocol) bool {
			return p == config.APIOpenAICompletions || p == config.APIAnthropicMessages || p == config.APIOpenAIResponses
		}
		for name, alias := range cfg.Models {
			if !known(alias.API) {
				t.Fatalf("alias %q has unknown protocol %q", name, alias.API)
			}
			positive := false
			for _, target := range alias.Targets {
				if _, ok := cfg.Upstreams[target.Upstream]; !ok {
					t.Fatalf("alias %q references absent upstream", name)
				}
				if target.Weight < 0 {
					t.Fatal("negative weight accepted")
				}
				positive = positive || target.Weight > 0
				p := target.API
				if p == "" {
					p = alias.API
				}
				if !known(p) || !(p == alias.API || alias.API == config.APIOpenAICompletions && p == config.APIAnthropicMessages || alias.API == config.APIAnthropicMessages && p == config.APIOpenAICompletions) {
					t.Fatal("unsupported protocol pair accepted")
				}
			}
			if !positive {
				t.Fatalf("alias %q has no positive-weight target", name)
			}
		}
	})
}
