package sync

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/storage"

	// Register storage adapters
	_ "github.com/tawanorg/claude-sync/internal/storage/gcs"
	_ "github.com/tawanorg/claude-sync/internal/storage/r2"
	_ "github.com/tawanorg/claude-sync/internal/storage/s3"
)

const defaultWorkers = 10

type Syncer struct {
	storage      storage.Storage
	encryptor    *crypto.Encryptor
	state        *SyncState
	claudeDir    string
	syncPaths    []string
	remotePrefix string
	quiet        bool
	onProgress   ProgressFunc
	cfg          *config.Config
}

type SyncResult struct {
	Uploaded   []string
	Downloaded []string
	Deleted    []string
	Conflicts  []string
	Orphans    []string
	Errors     []error
}

type ProgressEvent struct {
	Action   string // "upload", "download", "delete", "encrypt", "decrypt", "scan"
	Path     string
	Size     int64
	Current  int
	Total    int
	Complete bool
	Error    error
}

type ProgressFunc func(event ProgressEvent)

func NewSyncer(cfg *config.Config, quiet bool) (*Syncer, error) {
	storageCfg := cfg.GetStorageConfig()
	store, err := storage.New(storageCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create storage client: %w", err)
	}

	enc, err := crypto.NewEncryptor(cfg.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create encryptor: %w", err)
	}

	state, err := LoadStateFromDir(cfg.StateDirPath())
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}

	return &Syncer{
		storage:      store,
		encryptor:    enc,
		state:        state,
		claudeDir:    cfg.LocalDir(),
		syncPaths:    cfg.PathsToSync(),
		remotePrefix: cfg.EffectiveRemotePrefix(),
		quiet:        quiet,
		cfg:          cfg,
	}, nil
}

// NewSyncerWith creates a Syncer with pre-built dependencies (for testing).
func NewSyncerWith(cfg *config.Config, store storage.Storage, enc *crypto.Encryptor, state *SyncState, claudeDir string, quiet bool) *Syncer {
	return &Syncer{
		storage:      store,
		encryptor:    enc,
		state:        state,
		claudeDir:    claudeDir,
		syncPaths:    cfg.PathsToSync(),
		remotePrefix: cfg.EffectiveRemotePrefix(),
		quiet:        quiet,
		cfg:          cfg,
	}
}

func (s *Syncer) LocalDir() string {
	return s.claudeDir
}

func (s *Syncer) SyncPaths() []string {
	if len(s.syncPaths) == 0 {
		return s.cfg.PathsToSync()
	}
	return s.syncPaths
}

func (s *Syncer) RemotePrefix() string {
	return s.remotePrefix
}

func (s *Syncer) SetProgressFunc(fn ProgressFunc) {
	s.onProgress = fn
}

func (s *Syncer) progress(event ProgressEvent) {
	if s.onProgress != nil {
		s.onProgress(event)
	}
}

func (s *Syncer) isExcluded(relPath string) bool {
	return s.cfg.IsExcluded(relPath)
}

func (s *Syncer) log(format string, args ...interface{}) {
	if !s.quiet {
		fmt.Printf(format+"\n", args...)
	}
}

func (s *Syncer) Push(ctx context.Context) (*SyncResult, error) {
	result := &SyncResult{}

	s.progress(ProgressEvent{Action: "scan", Path: "Detecting changes..."})

	changes, err := s.detectChanges()
	if err != nil {
		return nil, fmt.Errorf("failed to detect changes: %w", err)
	}

	if len(changes) == 0 {
		s.progress(ProgressEvent{Action: "scan", Complete: true})
		return result, nil
	}

	// Separate uploads from deletes
	var uploads, deletes []FileChange
	for _, change := range changes {
		switch change.Action {
		case "add", "modify":
			uploads = append(uploads, change)
		case "delete":
			deletes = append(deletes, change)
		}
	}

	total := len(changes)
	var mu sync.Mutex
	var completed atomic.Int32

	// Process uploads concurrently
	if len(uploads) > 0 {
		sem := make(chan struct{}, defaultWorkers)
		var wg sync.WaitGroup

		for _, change := range uploads {
			wg.Add(1)
			go func(change FileChange) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				n := int(completed.Add(1))
				s.progress(ProgressEvent{
					Action:  "upload",
					Path:    change.Path,
					Size:    change.LocalSize,
					Current: n,
					Total:   total,
				})

				if err := s.uploadFile(ctx, change.Path); err != nil {
					s.progress(ProgressEvent{
						Action: "upload",
						Path:   change.Path,
						Error:  err,
					})
					mu.Lock()
					result.Errors = append(result.Errors, fmt.Errorf("%s: %w", change.Path, err))
					mu.Unlock()
					return
				}
				mu.Lock()
				result.Uploaded = append(result.Uploaded, change.Path)
				mu.Unlock()
			}(change)
		}
		wg.Wait()
	}

	// Process deletes (use batch delete if available, otherwise concurrent)
	if len(deletes) > 0 {
		deleteKeys := make([]string, len(deletes))
		for i, change := range deletes {
			deleteKeys[i] = s.remoteKey(change.Path)
		}
		if err := s.storage.DeleteBatch(ctx, deleteKeys); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("batch delete: %w", err))
		} else {
			for _, change := range deletes {
				s.state.RemoveFile(change.Path)
				result.Deleted = append(result.Deleted, change.Path)
			}
		}
	}

	s.progress(ProgressEvent{Action: "upload", Complete: true, Total: total})

	s.state.LastPush = time.Now()
	s.state.LastSync = time.Now()
	if err := s.state.Save(); err != nil {
		return result, fmt.Errorf("failed to save state: %w", err)
	}

	return result, nil
}

func (s *Syncer) Pull(ctx context.Context) (*SyncResult, error) {
	result := &SyncResult{}

	s.progress(ProgressEvent{Action: "scan", Path: "Fetching remote file list..."})

	// List all remote objects
	remoteObjects, err := s.storage.List(ctx, s.remotePrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list remote objects: %w", err)
	}

	if len(remoteObjects) == 0 {
		// Still check for orphans when remote is completely empty
		remotePathSet := make(map[string]bool)
		result.Orphans = s.state.FindOrphans(remotePathSet)
		if len(result.Orphans) > 0 {
			s.state.LastPull = time.Now()
			s.state.LastSync = time.Now()
			s.state.Save()
		} else {
			s.progress(ProgressEvent{Action: "scan", Complete: true})
		}
		return result, nil
	}

	// Build remote file map
	remoteFiles := make(map[string]storage.ObjectInfo)
	for _, obj := range remoteObjects {
		// Skip non-encrypted files
		if !strings.HasSuffix(obj.Key, ".age") {
			continue
		}
		localPath := s.localPath(obj.Key)
		// Skip external files (handled by MCP sync)
		if strings.HasPrefix(localPath, "_external/") {
			continue
		}
		// Skip excluded paths
		if s.isExcluded(localPath) {
			continue
		}
		remoteFiles[localPath] = obj
	}

	// Get current local files
	localFiles, err := GetLocalFiles(s.claudeDir, s.SyncPaths(), s.isExcluded)
	if err != nil {
		return nil, fmt.Errorf("failed to get local files: %w", err)
	}

	// Build list of files to download
	type downloadTask struct {
		localPath string
		remoteObj storage.ObjectInfo
	}
	var toDownload []downloadTask

	for localPath, remoteObj := range remoteFiles {
		localInfo, localExists := localFiles[localPath]
		stateFile := s.state.GetFile(localPath)

		shouldDownload := false

		// Settings JSON with field-level merge: always pull and let merge sort it out.
		if s.isSettingsJSON(localPath) {
			if !localExists || (stateFile != nil && remoteObj.LastModified.After(stateFile.Uploaded)) || stateFile == nil {
				shouldDownload = true
			}
			if shouldDownload {
				toDownload = append(toDownload, downloadTask{localPath, remoteObj})
			}
			continue
		}

		if !localExists {
			if stateFile != nil {
				// File was pulled/pushed before but is now missing locally — user deleted it.
				// Skip re-download so a subsequent push propagates the deletion to remote.
				// Local intent wins even if remote was updated after our last sync.
				continue
			}
			shouldDownload = true
		} else if stateFile != nil {
			// Check if remote is newer than our last known state
			if remoteObj.LastModified.After(stateFile.Uploaded) {
				// Remote was updated after we last uploaded
				// Check if local was also modified
				localHash, _ := s.hashLocalFile(localPath)
				if localHash != stateFile.Hash {
					// Conflict: both changed
					result.Conflicts = append(result.Conflicts, localPath)
					s.progress(ProgressEvent{
						Action: "conflict",
						Path:   localPath,
					})
					if err := s.handleConflict(ctx, localPath, remoteObj); err != nil {
						result.Errors = append(result.Errors, err)
					}
					continue
				}
				shouldDownload = true
			}
		} else if localInfo.ModTime().Before(remoteObj.LastModified) {
			shouldDownload = true
		}

		if shouldDownload {
			toDownload = append(toDownload, downloadTask{localPath, remoteObj})
		}
	}

	// Download files concurrently
	total := len(toDownload)
	if total > 0 {
		sem := make(chan struct{}, defaultWorkers)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var completed atomic.Int32

		for _, task := range toDownload {
			wg.Add(1)
			go func(task downloadTask) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				n := int(completed.Add(1))
				s.progress(ProgressEvent{
					Action:  "download",
					Path:    task.localPath,
					Size:    task.remoteObj.Size,
					Current: n,
					Total:   total,
				})

				if err := s.downloadFile(ctx, task.localPath, task.remoteObj.Key); err != nil {
					s.progress(ProgressEvent{
						Action: "download",
						Path:   task.localPath,
						Error:  err,
					})
					mu.Lock()
					result.Errors = append(result.Errors, fmt.Errorf("%s: %w", task.localPath, err))
					mu.Unlock()
					return
				}
				mu.Lock()
				result.Downloaded = append(result.Downloaded, task.localPath)
				mu.Unlock()
			}(task)
		}
		wg.Wait()
	}

	s.progress(ProgressEvent{Action: "download", Complete: true, Total: total})

	// Detect orphans: files pulled before but no longer in remote
	remotePathSet := make(map[string]bool)
	for p := range remoteFiles {
		remotePathSet[p] = true
	}
	result.Orphans = s.state.FindOrphans(remotePathSet)

	s.state.LastPull = time.Now()
	s.state.LastSync = time.Now()
	if err := s.state.Save(); err != nil {
		return result, fmt.Errorf("failed to save state: %w", err)
	}

	return result, nil
}

func (s *Syncer) Status(ctx context.Context) ([]FileChange, error) {
	return s.detectChanges()
}

func (s *Syncer) uploadFile(ctx context.Context, relativePath string) error {
	fullPath := filepath.Join(s.claudeDir, relativePath)

	// Read file
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}
	data = s.prepareUploadData(relativePath, data)

	// Compress
	compressed, err := gzipCompress(data)
	if err != nil {
		return fmt.Errorf("failed to compress: %w", err)
	}

	// Encrypt
	encrypted, err := s.encryptor.Encrypt(compressed)
	if err != nil {
		return fmt.Errorf("failed to encrypt: %w", err)
	}

	// Upload
	remoteKey := s.remoteKey(relativePath)
	if err := s.storage.Upload(ctx, remoteKey, encrypted); err != nil {
		return fmt.Errorf("failed to upload: %w", err)
	}

	// Update state
	info, _ := os.Stat(fullPath)
	hash, _ := s.hashLocalFile(relativePath)
	s.state.UpdateFile(relativePath, info, hash)
	s.state.MarkUploaded(relativePath)
	s.state.SetOrigin(relativePath, OriginPush)

	if s.isSettingsJSON(relativePath) {
		if err := s.saveSettingsBaseline(); err != nil {
			return fmt.Errorf("failed to save settings baseline: %w", err)
		}
	}

	return nil
}

func (s *Syncer) downloadFile(ctx context.Context, relativePath, remoteKey string) error {
	data, err := s.fetchByRemoteKey(ctx, remoteKey)
	if err != nil {
		return err
	}

	// Ensure directory exists
	fullPath := filepath.Join(s.claudeDir, relativePath)
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Write file
	data, err = s.prepareDownloadData(relativePath, data)
	if err != nil {
		return err
	}
	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	// Update state
	info, _ := os.Stat(fullPath)
	hash, _ := s.hashLocalFile(relativePath)
	s.state.UpdateFile(relativePath, info, hash)
	s.state.MarkUploaded(relativePath)
	s.state.SetOrigin(relativePath, OriginPull)

	if s.isSettingsJSON(relativePath) {
		if err := s.saveSettingsBaseline(); err != nil {
			return fmt.Errorf("failed to save settings baseline: %w", err)
		}
	}

	return nil
}

func (s *Syncer) handleConflict(ctx context.Context, relativePath string, remoteObj storage.ObjectInfo) error {
	s.log("Conflict detected: %s (keeping local, saving remote as .conflict)", relativePath)

	// Download remote version with conflict suffix
	conflictPath := relativePath + ".conflict." + time.Now().Format("20060102-150405")
	if err := s.downloadFile(ctx, conflictPath, remoteObj.Key); err != nil {
		return fmt.Errorf("failed to save conflict file: %w", err)
	}

	return nil
}

func (s *Syncer) detectChanges() ([]FileChange, error) {
	return s.state.DetectChangesWithHash(s.claudeDir, s.SyncPaths(), s.hashLocalFile, s.isExcluded)
}

func (s *Syncer) hashLocalFile(relativePath string) (string, error) {
	if s.isSettingsJSON(relativePath) {
		return s.canonicalSettingsHash(relativePath)
	}
	fullPath := filepath.Join(s.claudeDir, relativePath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", err
	}
	if s.isCodexConfig(relativePath) {
		return hashCodexConfig(data), nil
	}
	return HashBytes(data), nil
}

func (s *Syncer) prepareUploadData(relativePath string, data []byte) []byte {
	if s.isCodexConfig(relativePath) {
		return sanitizeCodexConfig(data)
	}
	if s.isSettingsJSON(relativePath) {
		stripped, err := s.prepareSettingsForUpload(data)
		if err != nil {
			return data
		}
		return stripped
	}
	return data
}

func (s *Syncer) prepareDownloadData(relativePath string, data []byte) ([]byte, error) {
	if s.isSettingsJSON(relativePath) {
		return s.prepareSettingsForDownload(data)
	}
	if !s.isCodexConfig(relativePath) {
		return data, nil
	}
	fullPath := filepath.Join(s.claudeDir, relativePath)
	local, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return sanitizeCodexConfig(data), nil
		}
		return nil, fmt.Errorf("failed to read local codex config: %w", err)
	}
	return mergeCodexConfig(local, data), nil
}

func (s *Syncer) isCodexConfig(relativePath string) bool {
	return s.cfg.IsCodexProfile() && filepath.ToSlash(relativePath) == "config.toml"
}

func (s *Syncer) remoteKey(relativePath string) string {
	// Add .age extension for encrypted files
	return s.remotePrefix + relativePath + ".age"
}

// FetchRemoteContent downloads, decrypts and decompresses a remote file
// without writing it to disk. Returns the plaintext bytes.
func (s *Syncer) FetchRemoteContent(ctx context.Context, relativePath string) ([]byte, error) {
	return s.fetchByRemoteKey(ctx, s.remoteKey(relativePath))
}

func (s *Syncer) fetchByRemoteKey(ctx context.Context, remoteKey string) ([]byte, error) {
	encrypted, err := s.storage.Download(ctx, remoteKey)
	if err != nil {
		return nil, fmt.Errorf("failed to download: %w", err)
	}

	data, err := s.encryptor.Decrypt(encrypted)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}

	if isGzipped(data) {
		data, err = gzipDecompress(data)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress: %w", err)
		}
	}

	return data, nil
}

func (s *Syncer) localPath(remoteKey string) string {
	remoteKey = strings.TrimPrefix(remoteKey, s.remotePrefix)
	// Remove .age extension
	return strings.TrimSuffix(remoteKey, ".age")
}

func (s *Syncer) GetState() *SyncState {
	return s.state
}

// HasState returns true if the syncer has existing sync state (not first sync)
func (s *Syncer) HasState() bool {
	return !s.state.IsEmpty()
}

// FilePreview represents a file that would be affected by a pull operation
type FilePreview struct {
	Path       string
	LocalTime  time.Time
	RemoteTime time.Time
	LocalSize  int64
	RemoteSize int64
	LocalOnly  bool // File exists only locally
	RemoteOnly bool // File exists only remotely
}

// PullPreview represents what would happen during a pull operation
type PullPreview struct {
	WouldDownload       []FilePreview // New remote files that would be downloaded
	WouldOverwrite      []FilePreview // Existing local files that would be replaced
	WouldKeep           []FilePreview // Local files that would be kept (local newer)
	WouldConflict       []FilePreview // Files that would create a conflict
	LocalOnlyFiles      []FilePreview // Files that exist only locally
	OrphanedFiles       []FilePreview // Files pulled before, now deleted upstream
	WouldKeepDeleted    []FilePreview // Files deleted locally — pull skips them so push can delete remote
}

// PreviewPull returns a preview of what would happen during a pull operation
// without actually making any changes
func (s *Syncer) PreviewPull(ctx context.Context) (*PullPreview, error) {
	preview := &PullPreview{}

	// List all remote objects
	remoteObjects, err := s.storage.List(ctx, s.remotePrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list remote objects: %w", err)
	}

	// Build remote file map
	remoteFiles := make(map[string]storage.ObjectInfo)
	for _, obj := range remoteObjects {
		if !strings.HasSuffix(obj.Key, ".age") {
			continue
		}
		localPath := s.localPath(obj.Key)
		// Skip external files (handled by MCP sync)
		if strings.HasPrefix(localPath, "_external/") {
			continue
		}
		if s.isExcluded(localPath) {
			continue
		}
		remoteFiles[localPath] = obj
	}

	// Get current local files
	localFiles, err := GetLocalFiles(s.claudeDir, s.SyncPaths(), s.isExcluded)
	if err != nil {
		return nil, fmt.Errorf("failed to get local files: %w", err)
	}

	// Analyze each remote file
	for localPath, remoteObj := range remoteFiles {
		localInfo, localExists := localFiles[localPath]
		stateFile := s.state.GetFile(localPath)

		fp := FilePreview{
			Path:       localPath,
			RemoteTime: remoteObj.LastModified,
			RemoteSize: remoteObj.Size,
		}

		if localExists {
			fp.LocalTime = localInfo.ModTime()
			fp.LocalSize = localInfo.Size()
		}

		if !localExists {
			if stateFile != nil {
				// File was synced before but is now missing locally — user deleted it.
				// Pull will skip it; a subsequent push will propagate the deletion to remote.
				fp.LocalOnly = false
				preview.WouldKeepDeleted = append(preview.WouldKeepDeleted, fp)
				continue
			}
			// New file from remote
			fp.RemoteOnly = true
			preview.WouldDownload = append(preview.WouldDownload, fp)
		} else if stateFile != nil {
			// Check if remote is newer than our last known state
			if remoteObj.LastModified.After(stateFile.Uploaded) {
				// Remote was updated after we last uploaded
				localHash, _ := s.hashLocalFile(localPath)
				if localHash != stateFile.Hash {
					// Conflict: both changed
					preview.WouldConflict = append(preview.WouldConflict, fp)
				} else {
					// Only remote changed
					preview.WouldOverwrite = append(preview.WouldOverwrite, fp)
				}
			} else {
				// Local is current
				preview.WouldKeep = append(preview.WouldKeep, fp)
			}
		} else {
			// No state - compare timestamps
			if localInfo.ModTime().Before(remoteObj.LastModified) {
				preview.WouldOverwrite = append(preview.WouldOverwrite, fp)
			} else {
				preview.WouldKeep = append(preview.WouldKeep, fp)
			}
		}
	}

	// Find local-only and orphaned files
	for localPath, localInfo := range localFiles {
		if _, exists := remoteFiles[localPath]; !exists {
			if sf := s.state.GetFile(localPath); sf != nil && sf.Origin == OriginPull {
				preview.OrphanedFiles = append(preview.OrphanedFiles, FilePreview{
					Path:      localPath,
					LocalTime: localInfo.ModTime(),
					LocalSize: localInfo.Size(),
					LocalOnly: true,
				})
			} else {
				preview.LocalOnlyFiles = append(preview.LocalOnlyFiles, FilePreview{
					Path:      localPath,
					LocalTime: localInfo.ModTime(),
					LocalSize: localInfo.Size(),
					LocalOnly: true,
				})
			}
		}
	}

	return preview, nil
}

type DiffEntry struct {
	Path       string
	Status     string // "local_only", "remote_only", "modified", "synced"
	LocalSize  int64
	RemoteSize int64
	LocalTime  time.Time
	RemoteTime time.Time
}

func orphanStatus(relPath string, state *SyncState) string {
    if sf := state.GetFile(relPath); sf != nil && sf.Origin == OriginPull {
        return "orphaned"
    }
    return "local_only"
}

func (s *Syncer) Diff(ctx context.Context) ([]DiffEntry, error) {
	var entries []DiffEntry

	// Get local files
	localFiles, err := GetLocalFiles(s.claudeDir, s.SyncPaths(), s.isExcluded)
	if err != nil {
		return nil, fmt.Errorf("failed to get local files: %w", err)
	}

	// Get remote files
	remoteObjects, err := s.storage.List(ctx, s.remotePrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list remote objects: %w", err)
	}

	remoteFiles := make(map[string]storage.ObjectInfo)
	for _, obj := range remoteObjects {
		if !strings.HasSuffix(obj.Key, ".age") {
			continue
		}
		localPath := s.localPath(obj.Key)
		// Skip external files (handled by MCP sync)
		if strings.HasPrefix(localPath, "_external/") {
			continue
		}
		if s.isExcluded(localPath) {
			continue
		}
		remoteFiles[localPath] = obj
	}

	// Find local-only and modified files
	for relPath, info := range localFiles {
		remoteObj, exists := remoteFiles[relPath]
		if !exists {
			entries = append(entries, DiffEntry{
				Path:      relPath,
				Status:    orphanStatus(relPath, s.state),
				LocalSize: info.Size(),
				LocalTime: info.ModTime(),
			})
		} else {
			stateFile := s.state.GetFile(relPath)
			if stateFile != nil {
				localHash, _ := s.hashLocalFile(relPath)
				if localHash != stateFile.Hash || remoteObj.LastModified.After(stateFile.Uploaded) {
					entries = append(entries, DiffEntry{
						Path:       relPath,
						Status:     "modified",
						LocalSize:  info.Size(),
						RemoteSize: remoteObj.Size,
						LocalTime:  info.ModTime(),
						RemoteTime: remoteObj.LastModified,
					})
				} else {
					entries = append(entries, DiffEntry{
						Path:       relPath,
						Status:     "synced",
						LocalSize:  info.Size(),
						RemoteSize: remoteObj.Size,
						LocalTime:  info.ModTime(),
						RemoteTime: remoteObj.LastModified,
					})
				}
			} else {
				entries = append(entries, DiffEntry{
					Path:       relPath,
					Status:     "modified",
					LocalSize:  info.Size(),
					RemoteSize: remoteObj.Size,
					LocalTime:  info.ModTime(),
					RemoteTime: remoteObj.LastModified,
				})
			}
		}
	}

	// Find remote-only files
	for relPath, obj := range remoteFiles {
		if _, exists := localFiles[relPath]; !exists {
			entries = append(entries, DiffEntry{
				Path:       relPath,
				Status:     "remote_only",
				RemoteSize: obj.Size,
				RemoteTime: obj.LastModified,
			})
		}
	}

	return entries, nil
}

// claudeJSONPath returns the path to ~/.claude.json, respecting test overrides.
func (s *Syncer) claudeJSONPath() string {
	if s.cfg.ClaudeJSONOverride != "" {
		return s.cfg.ClaudeJSONOverride
	}
	return config.ClaudeJSONPath()
}

// PushMCP reads local MCP server configs, normalizes paths, and uploads them.
func (s *Syncer) PushMCP(ctx context.Context) (*MCPPushResult, error) {
	result := &MCPPushResult{}

	claudeJSON := s.claudeJSONPath()
	servers, err := ReadMCPServers(claudeJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to read MCP servers: %w", err)
	}
	if len(servers) == 0 {
		result.Unchanged = true
		return result, nil
	}

	homeDir, _ := os.UserHomeDir()
	normalized, err := NormalizeMCPServers(servers, homeDir)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize MCP paths: %w", err)
	}

	// Check if anything changed vs last push
	newHash, err := HashMCPServers(normalized)
	if err != nil {
		return nil, fmt.Errorf("failed to hash MCP servers: %w", err)
	}

	stateFile := s.state.GetFile(config.MCPRemoteKey)
	if stateFile != nil && stateFile.Hash == newHash {
		result.Unchanged = true
		return result, nil
	}

	// Serialize, compress, encrypt, upload
	data, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize MCP servers: %w", err)
	}

	compressed, err := gzipCompress(data)
	if err != nil {
		return nil, fmt.Errorf("failed to compress: %w", err)
	}

	encrypted, err := s.encryptor.Encrypt(compressed)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt: %w", err)
	}

	remoteKey := s.remoteKey(config.MCPRemoteKey)
	if err := s.storage.Upload(ctx, remoteKey, encrypted); err != nil {
		return nil, fmt.Errorf("failed to upload MCP servers: %w", err)
	}

	// Update state
	s.state.mu.Lock()
	s.state.Files[config.MCPRemoteKey] = &FileState{
		Path:     config.MCPRemoteKey,
		Hash:     newHash,
		Size:     int64(len(data)),
		ModTime:  time.Now(),
		Uploaded: time.Now(),
		Origin:   OriginPush,
	}
	s.state.mu.Unlock()

	if err := s.state.SetMCPBaseline(normalized); err != nil {
		return nil, fmt.Errorf("failed to save MCP baseline: %w", err)
	}

	if err := s.state.Save(); err != nil {
		return nil, fmt.Errorf("failed to save state: %w", err)
	}

	result.ServersPushed = len(normalized)
	return result, nil
}

// PullMCP downloads remote MCP server configs and merges them with local configs.
func (s *Syncer) PullMCP(ctx context.Context) (*MCPPullResult, error) {
	result := &MCPPullResult{}

	// Download remote MCP data
	remoteKey := s.remoteKey(config.MCPRemoteKey)
	encrypted, err := s.storage.Download(ctx, remoteKey)
	if err != nil {
		// If the key doesn't exist, no remote MCP data
		result.NoRemote = true
		return result, nil
	}

	decrypted, err := s.encryptor.Decrypt(encrypted)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt MCP data: %w", err)
	}

	if isGzipped(decrypted) {
		decrypted, err = gzipDecompress(decrypted)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress MCP data: %w", err)
		}
	}

	var remoteServers MCPServers
	if err := json.Unmarshal(decrypted, &remoteServers); err != nil {
		return nil, fmt.Errorf("failed to parse remote MCP servers: %w", err)
	}

	// Read local servers
	claudeJSON := s.claudeJSONPath()
	localServers, err := ReadMCPServers(claudeJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to read local MCP servers: %w", err)
	}
	if localServers == nil {
		localServers = make(MCPServers)
	}

	// Normalize local for comparison
	homeDir, _ := os.UserHomeDir()
	localNormalized, err := NormalizeMCPServers(localServers, homeDir)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize local MCP paths: %w", err)
	}

	// Load baseline
	baseline, err := s.state.GetMCPBaseline()
	if err != nil {
		return nil, fmt.Errorf("failed to load MCP baseline: %w", err)
	}
	if baseline == nil {
		baseline = make(MCPServers)
	}

	// Three-way merge
	mergeResult := MergeMCPServers(localNormalized, remoteServers, baseline)

	// Resolve paths in merged result
	resolved, err := ResolveMCPServers(mergeResult.Merged, homeDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve MCP paths: %w", err)
	}

	// Write merged result back to claude.json
	if err := WriteMCPServers(claudeJSON, resolved); err != nil {
		return nil, fmt.Errorf("failed to write MCP servers: %w", err)
	}

	// Update baseline to the merged normalized state
	if err := s.state.SetMCPBaseline(mergeResult.Merged); err != nil {
		return nil, fmt.Errorf("failed to save MCP baseline: %w", err)
	}

	// Update file state
	newHash, _ := HashMCPServers(mergeResult.Merged)
	s.state.mu.Lock()
	s.state.Files[config.MCPRemoteKey] = &FileState{
		Path:     config.MCPRemoteKey,
		Hash:     newHash,
		Size:     int64(len(decrypted)),
		ModTime:  time.Now(),
		Uploaded: time.Now(),
		Origin:   OriginPull,
	}
	s.state.mu.Unlock()

	if err := s.state.Save(); err != nil {
		return nil, fmt.Errorf("failed to save state: %w", err)
	}

	result.Added = mergeResult.Added
	result.Updated = mergeResult.Updated
	result.Kept = mergeResult.Kept
	result.Conflicts = mergeResult.Conflicts
	return result, nil
}

// MCPStatus returns the current state of local MCP servers compared to the last sync.
type MCPStatusResult struct {
	Servers     MCPServers
	HasChanges  bool
	ServerCount int
}

func (s *Syncer) MCPStatus(ctx context.Context) (*MCPStatusResult, error) {
	claudeJSON := s.claudeJSONPath()
	servers, err := ReadMCPServers(claudeJSON)
	if err != nil {
		return nil, fmt.Errorf("failed to read MCP servers: %w", err)
	}

	result := &MCPStatusResult{
		Servers:     servers,
		ServerCount: len(servers),
	}

	if servers == nil {
		return result, nil
	}

	homeDir, _ := os.UserHomeDir()
	normalized, err := NormalizeMCPServers(servers, homeDir)
	if err != nil {
		return nil, err
	}

	newHash, err := HashMCPServers(normalized)
	if err != nil {
		return nil, err
	}

	stateFile := s.state.GetFile(config.MCPRemoteKey)
	result.HasChanges = stateFile == nil || stateFile.Hash != newHash

	return result, nil
}

// PruneOrphans deletes orphaned files locally and removes them from state.
func (s *Syncer) PruneOrphans(orphans []string) error {
	for _, path := range orphans {
		fullPath := filepath.Join(s.claudeDir, path)
		if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove orphan %s: %w", path, err)
		}
		s.state.RemoveFile(path)
	}
	return s.state.Save()
}

// isGzipped checks if data starts with the gzip magic number (0x1f 0x8b).
func isGzipped(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}

func gzipCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gzipDecompress(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
