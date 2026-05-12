package sync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
)

func TestStripSettingsKeys(t *testing.T) {
	input := []byte(`{"theme":"dark","env":{"TOKEN":"secret","DEBUG":"1"},"autoSave":true}`)
	got, err := stripSettingsKeys(input, []string{"env.TOKEN"})
	if err != nil {
		t.Fatalf("stripSettingsKeys failed: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(got, &result); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}

	// env.TOKEN should be gone
	env, ok := result["env"].(map[string]any)
	if !ok {
		t.Fatal("env should still exist")
	}
	if _, exists := env["TOKEN"]; exists {
		t.Error("env.TOKEN should be stripped")
	}
	if env["DEBUG"] != "1" {
		t.Error("env.DEBUG should be preserved")
	}
	if result["theme"] != "dark" {
		t.Error("theme should be preserved")
	}
}

func TestStripTopLevelKey(t *testing.T) {
	input := []byte(`{"theme":"dark","env":{"TOKEN":"secret"}}`)
	got, err := stripSettingsKeys(input, []string{"env"})
	if err != nil {
		t.Fatalf("stripSettingsKeys failed: %v", err)
	}
	var result map[string]any
	json.Unmarshal(got, &result)

	if _, exists := result["env"]; exists {
		t.Error("env should be stripped entirely")
	}
	if result["theme"] != "dark" {
		t.Error("theme should be preserved")
	}
}

func TestCanonicalHashIgnoresKeyOrder(t *testing.T) {
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, ".claude-sync")
	claudeDir := filepath.Join(tmpDir, ".claude")
	os.MkdirAll(stateDir, 0700)
	os.MkdirAll(claudeDir, 0755)

	keyPath := filepath.Join(stateDir, "age-key.txt")
	crypto.GenerateKeyFromPassphrase(keyPath, "test")
	enc, _ := crypto.NewEncryptor(keyPath)
	state, _ := LoadStateFromDir(stateDir)

	cfg := &config.Config{
		SettingsSync: &config.SettingsSyncConfig{
			StripKeys: []string{"env.TOKEN"},
		},
	}
	s := &Syncer{
		storage:   newMockStorage(),
		encryptor: enc,
		state:     state,
		claudeDir: claudeDir,
		quiet:     true,
		cfg:       cfg,
	}

	// Write two JSON files that are semantically identical but different key order
	contentA := `{"env":{"TOKEN":"secret","DEBUG":"1"},"theme":"dark"}`
	contentB := `{"theme":"dark","env":{"DEBUG":"1","TOKEN":"secret"}}`

	os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(contentA), 0644)
	hashA, _ := s.canonicalSettingsHash("settings.json")

	os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(contentB), 0644)
	hashB, _ := s.canonicalSettingsHash("settings.json")

	if hashA != hashB {
		t.Errorf("hash should be key-order-independent:\n  A: %s\n  B: %s", hashA, hashB)
	}
}

func TestSettingsPushStripsKeys(t *testing.T) {
	tmpDir := t.TempDir()
	claudeDir := filepath.Join(tmpDir, ".claude")
	stateDir := filepath.Join(tmpDir, ".claude-sync")
	os.MkdirAll(claudeDir, 0755)
	os.MkdirAll(stateDir, 0700)

	keyPath := filepath.Join(stateDir, "age-key.txt")
	crypto.GenerateKeyFromPassphrase(keyPath, "test")
	enc, _ := crypto.NewEncryptor(keyPath)
	state, _ := LoadStateFromDir(stateDir)
	store := newMockStorage()

	s := &Syncer{
		storage:   store,
		encryptor: enc,
		state:     state,
		claudeDir: claudeDir,
		quiet:     true,
		cfg: &config.Config{
			SettingsSync: &config.SettingsSyncConfig{
				StripKeys: []string{"env.TOKEN"},
			},
		},
	}

	settingsJSON := `{"theme":"dark","env":{"TOKEN":"secret123","DEBUG":"1"}}`
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settingsJSON), 0644)

	ctx := context.Background()
	result, err := s.Push(ctx)
	if err != nil {
		t.Fatalf("Push failed: %v", err)
	}
	if len(result.Uploaded) != 1 {
		t.Fatalf("Expected 1 upload, got %d", len(result.Uploaded))
	}

	// Download and check what was actually stored
	remoteData, err := s.FetchRemoteContent(ctx, "settings.json")
	if err != nil {
		t.Fatalf("FetchRemoteContent failed: %v", err)
	}

	var stored map[string]any
	json.Unmarshal(remoteData, &stored)

	env, ok := stored["env"].(map[string]any)
	if !ok {
		t.Fatal("env should exist in stored data")
	}
	if _, exists := env["TOKEN"]; exists {
		t.Error("env.TOKEN should be stripped before upload")
	}
	if env["DEBUG"] != "1" {
		t.Errorf("env.DEBUG should be '1', got %v", env["DEBUG"])
	}
}

func TestSettingsPullMergePreservesStrippedKeys(t *testing.T) {
	tmpDir := t.TempDir()
	claudeDir := filepath.Join(tmpDir, ".claude")
	stateDir := filepath.Join(tmpDir, ".claude-sync")
	os.MkdirAll(claudeDir, 0755)
	os.MkdirAll(stateDir, 0700)

	stripKeys := []string{"env.TOKEN"}
	keyPath := filepath.Join(stateDir, "age-key.txt")
	crypto.GenerateKeyFromPassphrase(keyPath, "test")
	enc, _ := crypto.NewEncryptor(keyPath)
	state, _ := LoadStateFromDir(stateDir)
	store := newMockStorage()

	cfg := &config.Config{
		SettingsSync: &config.SettingsSyncConfig{
			StripKeys: stripKeys,
		},
	}

	s := &Syncer{
		storage:   store,
		encryptor: enc,
		state:     state,
		claudeDir: claudeDir,
		quiet:     true,
		cfg:       cfg,
	}

	// Local has token and theme
	localJSON := `{"theme":"dark","env":{"TOKEN":"my-secret","DEBUG":"1"}}`
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(localJSON), 0644)

	// Push to establish baseline
	ctx := context.Background()
	if _, err := s.Push(ctx); err != nil {
		t.Fatalf("Initial push failed: %v", err)
	}

	// Simulate remote change: another device changed theme to "light"
	remoteJSON := `{"theme":"light","env":{"DEBUG":"0"}}`
	remoteData, _ := s.encryptor.Encrypt([]byte(remoteJSON))
	store.Upload(ctx, "settings.json.age", remoteData)

	// Modify local too: change DEBUG to "2"
	localChanged := `{"theme":"dark","env":{"TOKEN":"my-secret","DEBUG":"2"}}`
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(localChanged), 0644)

	// Pull — should merge
	result, err := s.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}
	if len(result.Downloaded) != 1 {
		t.Fatalf("Expected 1 download, got %d", len(result.Downloaded))
	}

	// Read result
	got := readFile(t, claudeDir, "settings.json")
	var merged map[string]any
	json.Unmarshal([]byte(got), &merged)

	// Local's TOKEN should be preserved (not overwritten by remote which lacks it)
	env := merged["env"].(map[string]any)
	if env["TOKEN"] != "my-secret" {
		t.Errorf("env.TOKEN should be preserved from local, got %v", env["TOKEN"])
	}
	// Local DEBUG=2 is preserved (locally changed, not in baseline)
	if env["DEBUG"] != "2" {
		t.Errorf("env.DEBUG should be '2' (local change), got %v", env["DEBUG"])
	}
	// Remote theme=light should win (remote changed, local unchanged)
	if merged["theme"] != "light" {
		t.Errorf("theme should be 'light' (from remote), got %v", merged["theme"])
	}
}

func TestNoSettingsSyncWhenNotConfigured(t *testing.T) {
	tmpDir := t.TempDir()
	claudeDir := filepath.Join(tmpDir, ".claude")
	stateDir := filepath.Join(tmpDir, ".claude-sync")
	os.MkdirAll(claudeDir, 0755)
	os.MkdirAll(stateDir, 0700)

	keyPath := filepath.Join(stateDir, "age-key.txt")
	crypto.GenerateKeyFromPassphrase(keyPath, "test")
	enc, _ := crypto.NewEncryptor(keyPath)
	state, _ := LoadStateFromDir(stateDir)
	store := newMockStorage()

	// No SettingsSync config
	s := &Syncer{
		storage:   store,
		encryptor: enc,
		state:     state,
		claudeDir: claudeDir,
		quiet:     true,
		cfg:       &config.Config{},
	}

	settingsJSON := `{"theme":"dark","env":{"TOKEN":"secret"}}`
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settingsJSON), 0644)

	ctx := context.Background()
	_, err := s.Push(ctx)
	if err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	remoteData, _ := s.FetchRemoteContent(ctx, "settings.json")
	if !strings.Contains(string(remoteData), "secret") {
		t.Error("Without SettingsSync, TOKEN should NOT be stripped")
	}
}

func TestSettingsPushThenPullRoundTrip(t *testing.T) {
	// Device A pushes, Device B pulls — settings merge correctly
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, "shared-state")
	os.MkdirAll(stateDir, 0700)

	keyPath := filepath.Join(stateDir, "age-key.txt")
	crypto.GenerateKeyFromPassphrase(keyPath, "round-trip")
	enc, _ := crypto.NewEncryptor(keyPath)

	sharedStore := newMockStorage()

	stripKeys := []string{"env.TOKEN"}
	cfg := &config.Config{
		SettingsSync: &config.SettingsSyncConfig{
			StripKeys: stripKeys,
		},
	}

	// Device A
	dirA := filepath.Join(tmpDir, "deviceA", ".claude")
	stateDirA := filepath.Join(tmpDir, "deviceA", ".claude-sync")
	os.MkdirAll(dirA, 0755)
	os.MkdirAll(stateDirA, 0700)
	stateA, _ := LoadStateFromDir(stateDirA)
	sA := &Syncer{storage: sharedStore, encryptor: enc, state: stateA, claudeDir: dirA, quiet: true, cfg: cfg}

	localJSON := `{"theme":"dark","env":{"TOKEN":"secret-a","DEBUG":"1"}}`
	os.WriteFile(filepath.Join(dirA, "settings.json"), []byte(localJSON), 0644)

	ctx := context.Background()
	if _, err := sA.Push(ctx); err != nil {
		t.Fatalf("Device A push failed: %v", err)
	}

	// Verify remote has NO token
	remoteData, _ := sA.FetchRemoteContent(ctx, "settings.json")
	if strings.Contains(string(remoteData), "secret-a") {
		t.Fatal("Remote should NOT contain TOKEN value")
	}

	// Device B
	dirB := filepath.Join(tmpDir, "deviceB", ".claude")
	stateDirB := filepath.Join(tmpDir, "deviceB", ".claude-sync")
	os.MkdirAll(dirB, 0755)
	os.MkdirAll(stateDirB, 0700)
	stateB, _ := LoadStateFromDir(stateDirB)
	sB := &Syncer{storage: sharedStore, encryptor: enc, state: stateB, claudeDir: dirB, quiet: true, cfg: cfg}

	// Device B has its own TOKEN
	os.WriteFile(filepath.Join(dirB, "settings.json"), []byte(`{"env":{"TOKEN":"secret-b"}}`), 0644)

	resultB, err := sB.Pull(ctx)
	if err != nil {
		t.Fatalf("Device B pull failed: %v", err)
	}
	if len(resultB.Downloaded) != 1 {
		t.Fatalf("Expected 1 download, got %d", len(resultB.Downloaded))
	}

	got := readFile(t, dirB, "settings.json")
	var merged map[string]any
	json.Unmarshal([]byte(got), &merged)

	// Device B's TOKEN should be preserved
	env := merged["env"].(map[string]any)
	if env["TOKEN"] != "secret-b" {
		t.Errorf("Device B's TOKEN should be preserved, got %v", env["TOKEN"])
	}
	// Device A's theme and DEBUG should arrive
	if merged["theme"] != "dark" {
		t.Errorf("theme should be 'dark' from Device A, got %v", merged["theme"])
	}
	if env["DEBUG"] != "1" {
		t.Errorf("env.DEBUG should be '1' from Device A, got %v", env["DEBUG"])
	}
}
