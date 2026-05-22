package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/storage"
)

// mockStorage implements storage.Storage in-memory for testing.
type mockStorage struct {
	mu      sync.Mutex
	objects map[string]mockObject
}

type mockObject struct {
	data         []byte
	lastModified time.Time
}

func newMockStorage() *mockStorage {
	return &mockStorage{objects: make(map[string]mockObject)}
}

func (m *mockStorage) Upload(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.objects[key] = mockObject{data: cp, lastModified: time.Now()}
	return nil
}

func (m *mockStorage) Download(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("object not found: %s", key)
	}
	cp := make([]byte, len(obj.data))
	copy(cp, obj.data)
	return cp, nil
}

func (m *mockStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *mockStorage) DeleteBatch(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.objects, k)
	}
	return nil
}

func (m *mockStorage) List(_ context.Context, prefix string) ([]storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []storage.ObjectInfo
	for key, obj := range m.objects {
		if strings.HasPrefix(key, prefix) {
			result = append(result, storage.ObjectInfo{
				Key:          key,
				Size:         int64(len(obj.data)),
				LastModified: obj.lastModified,
			})
		}
	}
	return result, nil
}

func (m *mockStorage) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("object not found: %s", key)
	}
	return &storage.ObjectInfo{
		Key:          key,
		Size:         int64(len(obj.data)),
		LastModified: obj.lastModified,
	}, nil
}

func (m *mockStorage) BucketExists(_ context.Context) (bool, error) {
	return true, nil
}

// helper to create a test syncer with mock storage and temp dirs
type testEnv struct {
	syncer    *Syncer
	store     *mockStorage
	claudeDir string
	stateDir  string
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tmpDir := t.TempDir()
	claudeDir := filepath.Join(tmpDir, ".claude")
	stateDir := filepath.Join(tmpDir, ".claude-sync")

	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatalf("Failed to create claude dir: %v", err)
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("Failed to create state dir: %v", err)
	}

	// Generate encryption key
	keyPath := filepath.Join(stateDir, "age-key.txt")
	if err := crypto.GenerateKeyFromPassphrase(keyPath, "test-passphrase"); err != nil {
		t.Fatalf("Failed to generate key: %v", err)
	}
	enc, err := crypto.NewEncryptor(keyPath)
	if err != nil {
		t.Fatalf("Failed to create encryptor: %v", err)
	}

	state, err := LoadStateFromDir(stateDir)
	if err != nil {
		t.Fatalf("Failed to load state: %v", err)
	}

	store := newMockStorage()
	syncer := &Syncer{
		storage:   store,
		encryptor: enc,
		state:     state,
		claudeDir: claudeDir,
		quiet:     true,
		cfg:       &config.Config{},
	}

	return &testEnv{
		syncer:    syncer,
		store:     store,
		claudeDir: claudeDir,
		stateDir:  stateDir,
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("Failed to create dir for %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write %s: %v", name, err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("Failed to read %s: %v", name, err)
	}
	return string(data)
}

func TestPushUploadsNewFiles(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeFile(t, env.claudeDir, "CLAUDE.md", "# My Settings")
	writeFile(t, env.claudeDir, "settings.json", `{"theme":"dark"}`)

	result, err := env.syncer.Push(ctx)
	if err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	if len(result.Uploaded) != 2 {
		t.Errorf("Expected 2 uploads, got %d: %v", len(result.Uploaded), result.Uploaded)
	}
	if len(result.Errors) > 0 {
		t.Errorf("Unexpected errors: %v", result.Errors)
	}

	// Verify files exist in mock storage with .age suffix
	objs, _ := env.store.List(ctx, "")
	if len(objs) != 2 {
		t.Errorf("Expected 2 objects in storage, got %d", len(objs))
	}
	for _, obj := range objs {
		if !strings.HasSuffix(obj.Key, ".age") {
			t.Errorf("Expected .age suffix on key %s", obj.Key)
		}
	}
}

func TestPushUploadsModifiedFiles(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeFile(t, env.claudeDir, "CLAUDE.md", "# V1")

	// Initial push
	result, err := env.syncer.Push(ctx)
	if err != nil {
		t.Fatalf("Initial push failed: %v", err)
	}
	if len(result.Uploaded) != 1 {
		t.Fatalf("Expected 1 upload, got %d", len(result.Uploaded))
	}

	// Modify the file
	writeFile(t, env.claudeDir, "CLAUDE.md", "# V2 - modified")

	// Second push
	result, err = env.syncer.Push(ctx)
	if err != nil {
		t.Fatalf("Second push failed: %v", err)
	}
	if len(result.Uploaded) != 1 {
		t.Errorf("Expected 1 modified upload, got %d", len(result.Uploaded))
	}
}

func TestPushDeletesRemovedFiles(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeFile(t, env.claudeDir, "CLAUDE.md", "# Settings")

	// Push
	if _, err := env.syncer.Push(ctx); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// Delete local file
	os.Remove(filepath.Join(env.claudeDir, "CLAUDE.md"))

	// Push again
	result, err := env.syncer.Push(ctx)
	if err != nil {
		t.Fatalf("Delete push failed: %v", err)
	}
	if len(result.Deleted) != 1 {
		t.Errorf("Expected 1 delete, got %d", len(result.Deleted))
	}

	// Verify removed from storage
	objs, _ := env.store.List(ctx, "")
	if len(objs) != 0 {
		t.Errorf("Expected 0 objects in storage after delete, got %d", len(objs))
	}
}

func TestPullDownloadsNewRemoteFiles(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Simulate remote files by encrypting and uploading directly to mock storage
	content := []byte("# Remote Settings")
	encrypted, err := env.syncer.encryptor.Encrypt(content)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	if err := env.store.Upload(ctx, "CLAUDE.md.age", encrypted); err != nil {
		t.Fatalf("Upload to mock failed: %v", err)
	}

	// Pull
	result, err := env.syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}
	if len(result.Downloaded) != 1 {
		t.Errorf("Expected 1 download, got %d: %v", len(result.Downloaded), result.Downloaded)
	}

	// Verify local file
	got := readFile(t, env.claudeDir, "CLAUDE.md")
	if got != "# Remote Settings" {
		t.Errorf("Expected '# Remote Settings', got %q", got)
	}
}

func TestPullSkipsUnchangedFiles(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeFile(t, env.claudeDir, "CLAUDE.md", "# Synced")

	// Push to establish state
	if _, err := env.syncer.Push(ctx); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// Pull — nothing should be downloaded since remote hasn't changed beyond our push
	result, err := env.syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}
	if len(result.Downloaded) != 0 {
		t.Errorf("Expected 0 downloads (unchanged), got %d: %v", len(result.Downloaded), result.Downloaded)
	}
}

func TestPullDetectsConflicts(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeFile(t, env.claudeDir, "history.jsonl", `{"event":"local-v1"}`)

	// Push to establish baseline
	if _, err := env.syncer.Push(ctx); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// Modify local file (simulating local changes)
	writeFile(t, env.claudeDir, "history.jsonl", `{"event":"local-v2"}`)

	// Modify remote file (simulating another device pushing)
	remoteContent := []byte(`{"event":"remote-v2"}`)
	encrypted, err := env.syncer.encryptor.Encrypt(remoteContent)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	// Small delay to ensure remote timestamp is after the state's Uploaded time
	time.Sleep(10 * time.Millisecond)
	if err := env.store.Upload(ctx, "history.jsonl.age", encrypted); err != nil {
		t.Fatalf("Upload to mock failed: %v", err)
	}

	// Pull — should detect conflict
	result, err := env.syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}
	if len(result.Conflicts) != 1 {
		t.Errorf("Expected 1 conflict, got %d: %v", len(result.Conflicts), result.Conflicts)
	}

	// Local file should be preserved
	got := readFile(t, env.claudeDir, "history.jsonl")
	if got != `{"event":"local-v2"}` {
		t.Errorf("Local file should be preserved, got %q", got)
	}

	// A .conflict file should exist
	entries, err := os.ReadDir(env.claudeDir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	conflictFound := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "history.jsonl.conflict.") {
			conflictFound = true
			// Verify conflict file contains remote content
			data, _ := os.ReadFile(filepath.Join(env.claudeDir, e.Name()))
			if string(data) != `{"event":"remote-v2"}` {
				t.Errorf("Conflict file should contain remote content, got %q", string(data))
			}
		}
	}
	if !conflictFound {
		t.Error("Expected a .conflict file to be created")
	}
}

func TestNoConflictWhenOnlyRemoteChanged(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	writeFile(t, env.claudeDir, "CLAUDE.md", "# V1")

	// Push to establish baseline
	if _, err := env.syncer.Push(ctx); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// Remote changes (another device pushes a new version)
	remoteContent := []byte("# V2 from other device")
	encrypted, err := env.syncer.encryptor.Encrypt(remoteContent)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := env.store.Upload(ctx, "CLAUDE.md.age", encrypted); err != nil {
		t.Fatalf("Upload to mock failed: %v", err)
	}

	// Local file NOT modified (hash matches state)
	// Pull — should download without conflict
	result, err := env.syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Errorf("Expected 0 conflicts, got %d", len(result.Conflicts))
	}
	if len(result.Downloaded) != 1 {
		t.Errorf("Expected 1 download, got %d", len(result.Downloaded))
	}

	got := readFile(t, env.claudeDir, "CLAUDE.md")
	if got != "# V2 from other device" {
		t.Errorf("Expected remote content, got %q", got)
	}
}

func TestPushThenPullRoundTrip(t *testing.T) {
	// Device A pushes, Device B (fresh) pulls — content should match
	tmpDir := t.TempDir()
	stateDir := filepath.Join(tmpDir, "shared-state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("Failed to create state dir: %v", err)
	}

	// Shared encryption key
	keyPath := filepath.Join(stateDir, "age-key.txt")
	if err := crypto.GenerateKeyFromPassphrase(keyPath, "round-trip-test"); err != nil {
		t.Fatalf("Failed to generate key: %v", err)
	}
	enc, err := crypto.NewEncryptor(keyPath)
	if err != nil {
		t.Fatalf("Failed to create encryptor: %v", err)
	}

	sharedStore := newMockStorage()

	// Device A setup
	deviceADir := filepath.Join(tmpDir, "deviceA", ".claude")
	deviceAStateDir := filepath.Join(tmpDir, "deviceA", ".claude-sync")
	if err := os.MkdirAll(deviceADir, 0755); err != nil {
		t.Fatalf("Failed to create deviceA claude dir: %v", err)
	}
	if err := os.MkdirAll(deviceAStateDir, 0700); err != nil {
		t.Fatalf("Failed to create deviceA state dir: %v", err)
	}

	stateA, _ := LoadStateFromDir(deviceAStateDir)
	syncerA := &Syncer{
		storage:   sharedStore,
		encryptor: enc,
		state:     stateA,
		claudeDir: deviceADir,
		quiet:     true,
		cfg:       &config.Config{},
	}

	// Device A creates files and pushes
	writeFile(t, deviceADir, "CLAUDE.md", "# Shared config")
	writeFile(t, deviceADir, "settings.json", `{"theme":"dark","fontSize":14}`)
	writeFile(t, deviceADir, "agents/helper.json", `{"name":"helper","model":"opus"}`)

	ctx := context.Background()
	resultA, err := syncerA.Push(ctx)
	if err != nil {
		t.Fatalf("Device A push failed: %v", err)
	}
	if len(resultA.Uploaded) != 3 {
		t.Fatalf("Device A expected 3 uploads, got %d", len(resultA.Uploaded))
	}

	// Device B setup (fresh, no local files)
	deviceBDir := filepath.Join(tmpDir, "deviceB", ".claude")
	deviceBStateDir := filepath.Join(tmpDir, "deviceB", ".claude-sync")
	if err := os.MkdirAll(deviceBDir, 0755); err != nil {
		t.Fatalf("Failed to create deviceB claude dir: %v", err)
	}
	if err := os.MkdirAll(deviceBStateDir, 0700); err != nil {
		t.Fatalf("Failed to create deviceB state dir: %v", err)
	}

	stateB, _ := LoadStateFromDir(deviceBStateDir)
	syncerB := &Syncer{
		storage:   sharedStore,
		encryptor: enc,
		state:     stateB,
		claudeDir: deviceBDir,
		quiet:     true,
		cfg:       &config.Config{},
	}

	// Device B pulls
	resultB, err := syncerB.Pull(ctx)
	if err != nil {
		t.Fatalf("Device B pull failed: %v", err)
	}
	if len(resultB.Downloaded) != 3 {
		t.Errorf("Device B expected 3 downloads, got %d: %v", len(resultB.Downloaded), resultB.Downloaded)
	}

	// Verify content matches
	if got := readFile(t, deviceBDir, "CLAUDE.md"); got != "# Shared config" {
		t.Errorf("CLAUDE.md mismatch: %q", got)
	}
	if got := readFile(t, deviceBDir, "settings.json"); got != `{"theme":"dark","fontSize":14}` {
		t.Errorf("settings.json mismatch: %q", got)
	}
	if got := readFile(t, deviceBDir, "agents/helper.json"); got != `{"name":"helper","model":"opus"}` {
		t.Errorf("agents/helper.json mismatch: %q", got)
	}
}

func TestConflictCreatesConflictFile(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Push initial version of history.jsonl
	writeFile(t, env.claudeDir, "history.jsonl", "line1\n")
	if _, err := env.syncer.Push(ctx); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	// Local appends
	writeFile(t, env.claudeDir, "history.jsonl", "line1\nline2-local\n")

	// Remote also changed
	remoteData := []byte("line1\nline2-remote\n")
	encrypted, _ := env.syncer.encryptor.Encrypt(remoteData)
	time.Sleep(10 * time.Millisecond)
	if err := env.store.Upload(ctx, "history.jsonl.age", encrypted); err != nil {
		t.Fatalf("Upload to mock failed: %v", err)
	}

	// Pull
	result, err := env.syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}

	// Should have conflict
	if len(result.Conflicts) != 1 {
		t.Fatalf("Expected 1 conflict, got %d", len(result.Conflicts))
	}
	if result.Conflicts[0] != "history.jsonl" {
		t.Errorf("Expected conflict on history.jsonl, got %s", result.Conflicts[0])
	}

	// Local preserved
	local := readFile(t, env.claudeDir, "history.jsonl")
	if local != "line1\nline2-local\n" {
		t.Errorf("Local should be preserved, got %q", local)
	}

	// Conflict file has remote content
	entries, _ := os.ReadDir(env.claudeDir)
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), "history.jsonl.conflict.") {
			found = true
			data, _ := os.ReadFile(filepath.Join(env.claudeDir, e.Name()))
			if string(data) != "line1\nline2-remote\n" {
				t.Errorf("Conflict file content mismatch: %q", string(data))
			}
		}
	}
	if !found {
		t.Error("No .conflict file created")
	}
}

func TestPushNoChangesIsNoop(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Push with no files — should be a no-op
	result, err := env.syncer.Push(ctx)
	if err != nil {
		t.Fatalf("Push failed: %v", err)
	}
	if len(result.Uploaded) != 0 {
		t.Errorf("Expected 0 uploads, got %d", len(result.Uploaded))
	}
	if len(result.Deleted) != 0 {
		t.Errorf("Expected 0 deletes, got %d", len(result.Deleted))
	}
}

func TestPullEmptyRemoteIsNoop(t *testing.T) {
	env := setupTestEnv(t)
	ctx := context.Background()

	// Pull with nothing in remote
	result, err := env.syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}
	if len(result.Downloaded) != 0 {
		t.Errorf("Expected 0 downloads, got %d", len(result.Downloaded))
	}
}

func TestPullDetectsOrphans(t *testing.T) {
	ctx := context.Background()

	// Device A: create file and push
	envA := setupTestEnv(t)
	writeFile(t, envA.claudeDir, "CLAUDE.md", "# Device A")
	result, err := envA.syncer.Push(ctx)
	if err != nil {
		t.Fatalf("Push A failed: %v", err)
	}
	if len(result.Uploaded) != 1 {
		t.Fatalf("Expected 1 upload, got %d", len(result.Uploaded))
	}
	// Verify Origin is "push" after push
	sf := envA.syncer.state.GetFile("CLAUDE.md")
	if sf.Origin != OriginPush {
		t.Errorf("Expected Origin=push after push, got %q", sf.Origin)
	}

	// Device B: pull the file (simulate another device)
	envB := setupTestEnv(t)
	envB.syncer.storage = envA.store // share same storage
	result, err = envB.syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull B failed: %v", err)
	}
	if len(result.Downloaded) != 1 {
		t.Fatalf("Expected 1 download, got %d", len(result.Downloaded))
	}
	// Verify Origin is "pull" after pull on device B
	sfB := envB.syncer.state.GetFile("CLAUDE.md")
	if sfB.Origin != OriginPull {
		t.Errorf("Expected Origin=pull after pull, got %q", sfB.Origin)
	}
	// Verify file exists locally on B
	if _, err := os.Stat(filepath.Join(envB.claudeDir, "CLAUDE.md")); os.IsNotExist(err) {
		t.Fatal("CLAUDE.md should exist on device B after pull")
	}

	// Device A: delete the file locally and push the delete
	os.Remove(filepath.Join(envA.claudeDir, "CLAUDE.md"))
	result, err = envA.syncer.Push(ctx)
	if err != nil {
		t.Fatalf("Push delete failed: %v", err)
	}
	if len(result.Deleted) != 1 {
		t.Fatalf("Expected 1 delete, got %d", len(result.Deleted))
	}

	// Device B: pull again — should detect orphan
	result, err = envB.syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull B (second) failed: %v", err)
	}
	if len(result.Orphans) != 1 {
		t.Errorf("Expected 1 orphan, got %d", len(result.Orphans))
	}
	if len(result.Orphans) > 0 && result.Orphans[0] != "CLAUDE.md" {
		t.Errorf("Expected orphan CLAUDE.md, got %v", result.Orphans)
	}

	// File should still exist locally (not pruned)
	if _, err := os.Stat(filepath.Join(envB.claudeDir, "CLAUDE.md")); os.IsNotExist(err) {
		t.Fatal("CLAUDE.md should still exist — orphan reported but not auto-deleted")
	}
}

func TestPullPreservesLocalDeletion(t *testing.T) {
	ctx := context.Background()

	// Two devices share storage. A pushes a file; B pulls it.
	envA := setupTestEnv(t)
	writeFile(t, envA.claudeDir, "rules/keep-me.md", "# original")
	if _, err := envA.syncer.Push(ctx); err != nil {
		t.Fatalf("Push A failed: %v", err)
	}

	envB := setupTestEnv(t)
	envB.syncer.storage = envA.store
	if _, err := envB.syncer.Pull(ctx); err != nil {
		t.Fatalf("Pull B failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(envB.claudeDir, "rules/keep-me.md")); err != nil {
		t.Fatalf("file should exist on B after pull: %v", err)
	}

	// User on B deletes the file locally and runs sync (pull then push).
	if err := os.Remove(filepath.Join(envB.claudeDir, "rules/keep-me.md")); err != nil {
		t.Fatalf("local delete failed: %v", err)
	}

	pullRes, err := envB.syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull B (after delete) failed: %v", err)
	}
	if len(pullRes.Downloaded) != 0 {
		t.Errorf("expected 0 downloads (local deletion should win), got %d: %v",
			len(pullRes.Downloaded), pullRes.Downloaded)
	}
	if _, err := os.Stat(filepath.Join(envB.claudeDir, "rules/keep-me.md")); !os.IsNotExist(err) {
		t.Fatal("locally-deleted file must not be re-downloaded by pull")
	}

	// Push should propagate the deletion to remote.
	pushRes, err := envB.syncer.Push(ctx)
	if err != nil {
		t.Fatalf("Push B failed: %v", err)
	}
	if len(pushRes.Deleted) != 1 || pushRes.Deleted[0] != "rules/keep-me.md" {
		t.Errorf("expected push to delete rules/keep-me.md, got: %v", pushRes.Deleted)
	}

	// Remote should be empty.
	objs, _ := envA.store.List(ctx, "")
	if len(objs) != 0 {
		t.Errorf("expected remote empty after deletion sync, got %d objects", len(objs))
	}
}

func TestPreviewPullClassifiesLocalDeletion(t *testing.T) {
	ctx := context.Background()

	envA := setupTestEnv(t)
	writeFile(t, envA.claudeDir, "CLAUDE.md", "# v1")
	if _, err := envA.syncer.Push(ctx); err != nil {
		t.Fatalf("Push A failed: %v", err)
	}

	envB := setupTestEnv(t)
	envB.syncer.storage = envA.store
	if _, err := envB.syncer.Pull(ctx); err != nil {
		t.Fatalf("Pull B failed: %v", err)
	}
	os.Remove(filepath.Join(envB.claudeDir, "CLAUDE.md"))

	preview, err := envB.syncer.PreviewPull(ctx)
	if err != nil {
		t.Fatalf("PreviewPull failed: %v", err)
	}
	if len(preview.WouldDownload) != 0 {
		t.Errorf("expected no WouldDownload entries for locally-deleted file, got %d", len(preview.WouldDownload))
	}
	if len(preview.WouldKeepDeleted) != 1 || preview.WouldKeepDeleted[0].Path != "CLAUDE.md" {
		t.Errorf("expected CLAUDE.md in WouldKeepDeleted, got: %+v", preview.WouldKeepDeleted)
	}
}

func TestPruneOrphansDeletesFiles(t *testing.T) {
	ctx := context.Background()

	// Setup: push from A, pull on B, delete from A
	envA := setupTestEnv(t)
	writeFile(t, envA.claudeDir, "rules/test.md", "# test rule")
	envA.syncer.Push(ctx)

	envB := setupTestEnv(t)
	envB.syncer.storage = envA.store
	envB.syncer.Pull(ctx)

	os.Remove(filepath.Join(envA.claudeDir, "rules/test.md"))
	envA.syncer.Push(ctx)

	// Pull on B to detect orphan
	result, err := envB.syncer.Pull(ctx)
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}
	if len(result.Orphans) != 1 {
		t.Fatalf("Expected 1 orphan, got %d", len(result.Orphans))
	}

	// Prune
	if err := envB.syncer.PruneOrphans(result.Orphans); err != nil {
		t.Fatalf("PruneOrphans failed: %v", err)
	}

	// File should be deleted
	if _, err := os.Stat(filepath.Join(envB.claudeDir, "rules/test.md")); !os.IsNotExist(err) {
		t.Fatal("rules/test.md should be deleted after prune")
	}

	// State should be cleaned
	if sf := envB.syncer.state.GetFile("rules/test.md"); sf != nil {
		t.Error("State should not have entry for pruned file")
	}
}

func TestDiffShowsOrphanedStatus(t *testing.T) {
	ctx := context.Background()

	// Push from A, pull on B, delete from A
	envA := setupTestEnv(t)
	writeFile(t, envA.claudeDir, "CLAUDE.md", "# from A")
	envA.syncer.Push(ctx)

	envB := setupTestEnv(t)
	envB.syncer.storage = envA.store
	envB.syncer.Pull(ctx)

	os.Remove(filepath.Join(envA.claudeDir, "CLAUDE.md"))
	envA.syncer.Push(ctx)

	// Diff on B should show "orphaned", not "local_only"
	entries, err := envB.syncer.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}

	for _, e := range entries {
		if e.Path == "CLAUDE.md" {
			if e.Status != "orphaned" {
				t.Errorf("Expected status 'orphaned' for CLAUDE.md, got %q", e.Status)
			}
			return
		}
	}
	t.Error("CLAUDE.md not found in diff entries")
}

func TestLocalOnlyNotFlaggedAsOrphan(t *testing.T) {
	ctx := context.Background()

	env := setupTestEnv(t)
	writeFile(t, env.claudeDir, "CLAUDE.md", "# brand new")

	// File exists locally but never synced — should be "local_only", not "orphaned"
	entries, err := env.syncer.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff failed: %v", err)
	}

	for _, e := range entries {
		if e.Path == "CLAUDE.md" {
			if e.Status != "local_only" {
				t.Errorf("Unsynchronized local file should be 'local_only', got %q", e.Status)
			}
			return
		}
	}
	t.Error("CLAUDE.md not found in diff entries")
}
