package sync

import (
	"strings"
	"testing"
)

func TestSanitizeCodexConfig(t *testing.T) {
	input := []byte(`model = "gpt-5.5"
model_reasoning_effort = "medium"

[projects."/Users/alice/app"]
trust_level = "trusted"

[marketplaces.openai-bundled]
source_type = "local"
source = "/Users/alice/.codex/.tmp/bundled"

[plugins."browser-use@openai-bundled"]
enabled = true

[tui]
status_line = ["model-with-reasoning"]
`)

	got := string(sanitizeCodexConfig(input))
	if strings.Contains(got, "[projects.") {
		t.Fatalf("sanitized config should not include projects: %s", got)
	}
	if strings.Contains(got, "[marketplaces.") || strings.Contains(got, "/Users/alice") {
		t.Fatalf("sanitized config should not include marketplace local paths: %s", got)
	}
	if !strings.Contains(got, `model = "gpt-5.5"`) {
		t.Fatalf("sanitized config should keep top-level settings: %s", got)
	}
	if !strings.Contains(got, `[plugins."browser-use@openai-bundled"]`) {
		t.Fatalf("sanitized config should keep plugin settings: %s", got)
	}
	if !strings.Contains(got, "[tui]") {
		t.Fatalf("sanitized config should keep tui settings: %s", got)
	}
}

func TestMergeCodexConfigPreservesLocalDirectoryTables(t *testing.T) {
	local := []byte(`model = "gpt-5.4"

[projects."/Users/alice/app"]
trust_level = "trusted"

[marketplaces.openai-bundled]
source = "/Users/alice/.codex/.tmp/bundled"

[plugins."browser-use@openai-bundled"]
enabled = false
`)
	remote := []byte(`model = "gpt-5.5"
model_reasoning_effort = "medium"

[plugins."browser-use@openai-bundled"]
enabled = true

[tui]
status_line = ["model-with-reasoning"]
`)

	got := string(mergeCodexConfig(local, remote))
	if !strings.Contains(got, `model = "gpt-5.5"`) {
		t.Fatalf("merged config should use remote generic settings: %s", got)
	}
	if strings.Contains(got, `model = "gpt-5.4"`) {
		t.Fatalf("merged config should replace local generic settings: %s", got)
	}
	if !strings.Contains(got, `[projects."/Users/alice/app"]`) {
		t.Fatalf("merged config should preserve local project tables: %s", got)
	}
	if !strings.Contains(got, `source = "/Users/alice/.codex/.tmp/bundled"`) {
		t.Fatalf("merged config should preserve local marketplace paths: %s", got)
	}
	if !strings.Contains(got, "enabled = true") {
		t.Fatalf("merged config should use remote plugin settings: %s", got)
	}
}

func TestHashCodexConfigIgnoresLocalDirectoryTables(t *testing.T) {
	a := []byte(`model = "gpt-5.5"

[projects."/Users/alice/app"]
trust_level = "trusted"
`)
	b := []byte(`model = "gpt-5.5"

[projects."/Users/bob/app"]
trust_level = "trusted"

[marketplaces.openai-bundled]
source = "/Users/bob/.codex/cache"
`)

	if hashCodexConfig(a) != hashCodexConfig(b) {
		t.Fatal("codex config hash should ignore local directory tables")
	}
}
