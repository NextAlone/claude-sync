package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tawanorg/claude-sync/internal/storage"
	"gopkg.in/yaml.v3"
)

const (
	ConfigDir  = ".claude-sync"
	ConfigFile = "config.yaml"
	StateFile  = "state.json"
	AgeKeyFile = "age-key.txt"

	// MCPRemoteKey is the remote storage key for synced MCP server configs.
	// The _external/ prefix separates it from ~/.claude/-relative files.
	MCPRemoteKey = "_external/mcp-servers.json"
)

type Config struct {
	// New storage configuration (preferred)
	Storage *storage.StorageConfig `yaml:"storage,omitempty"`

	// Legacy R2-only fields (for backward compatibility)
	AccountID       string `yaml:"account_id,omitempty"`
	AccessKeyID     string `yaml:"access_key_id,omitempty"`
	SecretAccessKey string `yaml:"secret_access_key,omitempty"`
	Bucket          string `yaml:"bucket,omitempty"`
	Endpoint        string `yaml:"endpoint,omitempty"`

	// Common fields
	EncryptionKey string `yaml:"encryption_key_path"`

	// AppName labels the synced application in user-facing output.
	AppName string `yaml:"app_name,omitempty"`

	// SourceDir is the local directory to sync. Defaults to ~/.claude.
	SourceDir string `yaml:"source_dir,omitempty"`

	// StateDir stores sync metadata. Defaults to ~/.claude-sync.
	StateDir string `yaml:"state_dir,omitempty"`

	// RemotePrefix isolates objects in shared buckets, e.g. "codex/".
	RemotePrefix string `yaml:"remote_prefix,omitempty"`

	// SyncPaths overrides the paths under SourceDir that should be synced.
	SyncPaths []string `yaml:"sync_paths,omitempty"`

	// Exclude patterns (glob-style) for paths to skip during sync
	Exclude []string `yaml:"exclude,omitempty"`

	// MCPSync enables syncing MCP server configs from ~/.claude.json
	MCPSync bool `yaml:"mcp_sync,omitempty"`

	// ClaudeDirOverride allows overriding the default ~/.claude path (for testing)
	ClaudeDirOverride string `yaml:"-"`

	// StateDirOverride allows overriding the state file directory (for testing)
	StateDirOverride string `yaml:"-"`

	// ClaudeJSONOverride allows overriding the ~/.claude.json path (for testing)
	ClaudeJSONOverride string `yaml:"-"`
}

// SyncPaths defines which paths under ~/.claude to sync
var SyncPaths = []string{
	"CLAUDE.md",
	"settings.json",
	"settings.local.json",
	"agents",
	"commands",
	"skills",
	"plugins",
	"projects",
	"plans",
	"tasks",
	"history.jsonl",
	"rules",
	"statusline.sh",
	"hooks",
}

var CodexSyncPaths = []string{
	"AGENTS.md",
	"config.toml",
	"hooks.json",
	"skills",
	"plugins",
}

var CodexExclude = []string{
	"auth.json",
	"*.sqlite",
	"*.sqlite-*",
	"*.jsonl",
	".tmp/**",
	"cache/**",
	"log/**",
	"sessions/**",
	"shell_snapshots/**",
	"tmp/**",
	"vendor_imports/**",
	"models_cache.json",
}

func ConfigDirPath() string {
	if dir := os.Getenv("CLAUDE_SYNC_CONFIG_DIR"); dir != "" {
		if strings.HasPrefix(dir, "~") {
			home, _ := os.UserHomeDir()
			return filepath.Join(home, dir[1:])
		}
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ConfigDir)
}

func ConfigFilePath() string {
	return filepath.Join(ConfigDirPath(), ConfigFile)
}

func StateFilePath() string {
	return filepath.Join(ConfigDirPath(), StateFile)
}

func StateFilePathForDir(dir string) string {
	return filepath.Join(dir, StateFile)
}

func AgeKeyFilePath() string {
	return filepath.Join(ConfigDirPath(), AgeKeyFile)
}

func ClaudeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

func CodexDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// ClaudeJSONPath returns the path to ~/.claude.json where global MCP servers are configured.
func ClaudeJSONPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

func Load() (*Config, error) {
	configPath := ConfigFilePath()

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config not found: run init first")
		}
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Expand ~ in encryption key path
	if cfg.EncryptionKey != "" && cfg.EncryptionKey[0] == '~' {
		home, _ := os.UserHomeDir()
		cfg.EncryptionKey = filepath.Join(home, cfg.EncryptionKey[1:])
	}
	if cfg.SourceDir != "" && cfg.SourceDir[0] == '~' {
		home, _ := os.UserHomeDir()
		cfg.SourceDir = filepath.Join(home, cfg.SourceDir[1:])
	}
	if cfg.StateDir != "" && cfg.StateDir[0] == '~' {
		home, _ := os.UserHomeDir()
		cfg.StateDir = filepath.Join(home, cfg.StateDir[1:])
	}
	cfg.RemotePrefix = NormalizeRemotePrefix(cfg.RemotePrefix)

	// Set default endpoint for Cloudflare R2
	if cfg.Endpoint == "" && cfg.AccountID != "" {
		cfg.Endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)
	}

	return &cfg, nil
}

func Save(cfg *Config) error {
	configDir := ConfigDirPath()
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to serialize config: %w", err)
	}

	configPath := ConfigFilePath()
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	return nil
}

func Exists() bool {
	_, err := os.Stat(ConfigFilePath())
	return err == nil
}

func (c *Config) DisplayName() string {
	if c.AppName != "" {
		return c.AppName
	}
	return "Claude"
}

func (c *Config) LocalDir() string {
	if c.ClaudeDirOverride != "" {
		return c.ClaudeDirOverride
	}
	if c.SourceDir != "" {
		return c.SourceDir
	}
	return ClaudeDir()
}

func (c *Config) StateDirPath() string {
	if c.StateDirOverride != "" {
		return c.StateDirOverride
	}
	if c.StateDir != "" {
		return c.StateDir
	}
	return ConfigDirPath()
}

func (c *Config) PathsToSync() []string {
	if len(c.SyncPaths) > 0 {
		return c.SyncPaths
	}
	return SyncPaths
}

func (c *Config) EffectiveRemotePrefix() string {
	return NormalizeRemotePrefix(c.RemotePrefix)
}

func (c *Config) IsCodexProfile() bool {
	return strings.EqualFold(c.AppName, "Codex") ||
		strings.HasSuffix(filepath.ToSlash(c.SourceDir), "/.codex") ||
		c.EffectiveRemotePrefix() == "codex/"
}

func NormalizeRemotePrefix(prefix string) string {
	prefix = strings.TrimSpace(filepath.ToSlash(prefix))
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

// GetStorageConfig returns the storage configuration, migrating from legacy format if needed
func (c *Config) GetStorageConfig() *storage.StorageConfig {
	// If new format is already configured, use it
	if c.Storage != nil && c.Storage.Provider != "" {
		return c.Storage
	}

	// Migrate from legacy R2 format
	return &storage.StorageConfig{
		Provider:        storage.ProviderR2,
		Bucket:          c.Bucket,
		AccountID:       c.AccountID,
		AccessKeyID:     c.AccessKeyID,
		SecretAccessKey: c.SecretAccessKey,
		Endpoint:        c.Endpoint,
	}
}

// IsLegacyConfig returns true if using the legacy R2-only config format
func (c *Config) IsLegacyConfig() bool {
	return c.Storage == nil && c.AccountID != ""
}

// IsExcluded returns true if the given relative path matches any exclude pattern.
// Patterns support:
//   - filepath.Match glob syntax (e.g. "plugins/marketplace*", "*.tmp")
//   - Directory prefix (e.g. "plugins/marketplace" matches everything under it)
//   - Recursive wildcard (e.g. "plugins/cache/**" matches directory and all contents)
//   - Filename glob (e.g. "*.tmp" matches "foo/bar/file.tmp")
func (c *Config) IsExcluded(relPath string) bool {
	for _, pattern := range c.Exclude {
		// Handle "dir/**" pattern: match directory and everything under it
		if strings.HasSuffix(pattern, "/**") {
			dirPrefix := strings.TrimSuffix(pattern, "/**")
			if relPath == dirPrefix || strings.HasPrefix(relPath, dirPrefix+"/") {
				return true
			}
			continue
		}

		// Try glob match on full path
		matched, err := filepath.Match(pattern, relPath)
		if err == nil && matched {
			return true
		}

		// Try glob match on filename only (for patterns like "*.tmp")
		if strings.Contains(pattern, "*") || strings.Contains(pattern, "?") {
			if matched, _ := filepath.Match(pattern, filepath.Base(relPath)); matched {
				return true
			}
		}

		// Also match if the path starts with the pattern as a directory prefix
		// This lets "plugins/marketplace" exclude everything under that dir
		if len(relPath) > len(pattern) && relPath[:len(pattern)] == pattern &&
			(relPath[len(pattern)] == '/' || relPath[len(pattern)] == '\\') {
			return true
		}

		// Exact match
		if relPath == pattern {
			return true
		}
	}
	return false
}
