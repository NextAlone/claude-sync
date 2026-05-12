package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// isSettingsJSON returns true if relPath is "settings.json" and field-level sync is configured.
func (s *Syncer) isSettingsJSON(relPath string) bool {
	return filepath.ToSlash(relPath) == "settings.json" &&
		s.cfg.SettingsSync != nil &&
		len(s.cfg.SettingsSync.StripKeys) > 0
}

// stripSettingsKeys removes configured keys from settings JSON data.
// Returns the sanitized JSON (canonical key-sorted form).
func stripSettingsKeys(data []byte, stripKeys []string) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for _, key := range stripKeys {
		parts := strings.Split(key, ".")
		stripPath(raw, parts)
	}
	return canonicalJSONBytes(raw)
}

// stripPath removes a nested key path from a JSON object tree.
func stripPath(obj map[string]any, parts []string) {
	if len(parts) == 0 {
		return
	}
	if len(parts) == 1 {
		delete(obj, parts[0])
		return
	}
	if child, ok := obj[parts[0]]; ok {
		if childMap, ok := child.(map[string]any); ok {
			stripPath(childMap, parts[1:])
			if len(childMap) == 0 {
				delete(obj, parts[0])
			}
		}
	}
}

// canonicalJSONBytes marshals a value with sorted keys and no whitespace.
func canonicalJSONBytes(v any) ([]byte, error) {
	return json.Marshal(v)
}

// canonicalSettingsHash returns a hash of settings.json after stripping keys.
// This hash is key-order-independent.
func (s *Syncer) canonicalSettingsHash(relPath string) (string, error) {
	fullPath := filepath.Join(s.claudeDir, relPath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", err
	}
	stripped, err := stripSettingsKeys(data, s.cfg.SettingsSync.StripKeys)
	if err != nil {
		return "", err
	}
	return HashBytes(stripped), nil
}

// prepareSettingsForUpload strips sensitive keys and returns canonical JSON.
func (s *Syncer) prepareSettingsForUpload(data []byte) ([]byte, error) {
	return stripSettingsKeys(data, s.cfg.SettingsSync.StripKeys)
}

// settingsBaseline returns the last-synced canonical settings JSON for three-way merge.
func (s *Syncer) settingsBaseline() map[string]any {
	raw := s.state.GetSettingsBaseline()
	if raw == nil {
		return make(map[string]any)
	}
	return raw
}

// prepareSettingsForDownload merges remote settings with local, preserving stripped keys.
// Uses three-way merge: local vs remote vs baseline.
func (s *Syncer) prepareSettingsForDownload(remoteData []byte) ([]byte, error) {
	fullPath := filepath.Join(s.claudeDir, "settings.json")
	localData, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No local file — just use remote (but remote is already stripped)
			return remoteData, nil
		}
		return nil, err
	}

	var localRaw, remoteRaw map[string]any
	if err := json.Unmarshal(localData, &localRaw); err != nil {
		localRaw = make(map[string]any)
	}
	if err := json.Unmarshal(remoteData, &remoteRaw); err != nil {
		remoteRaw = make(map[string]any)
	}

	baselineRaw := s.settingsBaseline()

	// Three-way merge using mcp.go's mergeObjects
	merged, _ := mergeObjects(localRaw, remoteRaw, baselineRaw, "")

	// Restore stripped keys from local (they should never be overwritten by remote)
	for _, key := range s.cfg.SettingsSync.StripKeys {
		parts := strings.Split(key, ".")
		restorePath(localRaw, merged, parts)
	}

	return canonicalJSONBytes(merged)
}

// restorePath copies a value through a nested path from src to dst.
func restorePath(src, dst map[string]any, parts []string) {
	if len(parts) == 0 {
		return
	}
	if len(parts) == 1 {
		if v, ok := src[parts[0]]; ok {
			dst[parts[0]] = v
		}
		return
	}
	srcChild, srcOk := src[parts[0]]
	dstChild, dstOk := dst[parts[0]]
	if !srcOk || !dstOk {
		return
	}
	srcMap, srcIsMap := srcChild.(map[string]any)
	dstMap, dstIsMap := dstChild.(map[string]any)
	if srcIsMap && dstIsMap {
		restorePath(srcMap, dstMap, parts[1:])
	}
}

// saveSettingsBaseline stores the current sanitized local settings as the baseline
// after a successful sync (push or pull).
func (s *Syncer) saveSettingsBaseline() error {
	fullPath := filepath.Join(s.claudeDir, "settings.json")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return s.state.SetSettingsBaseline(nil)
		}
		return err
	}
	stripped, err := stripSettingsKeys(data, s.cfg.SettingsSync.StripKeys)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(stripped, &raw); err != nil {
		return err
	}
	return s.state.SetSettingsBaseline(raw)
}
