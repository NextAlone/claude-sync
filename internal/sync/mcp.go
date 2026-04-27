package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// MCPServers maps server names to their raw JSON configuration.
// Using json.RawMessage preserves unknown fields within each server entry.
type MCPServers map[string]json.RawMessage

// MCPMergeResult describes the outcome of a three-way merge of MCP server configs.
type MCPMergeResult struct {
	Merged    MCPServers
	Added     []string // server keys added from remote
	Updated   []string // server keys updated from remote
	Kept      []string // server keys kept as-is
	Conflicts []MCPConflict
}

// FieldConflict represents a conflict at a specific field path within a server config.
type FieldConflict struct {
	Path   string // Field path, e.g., "args[0]" or "env.FOO"
	Local  any
	Remote any
}

// MCPConflict represents a merge conflict for a single MCP server key.
type MCPConflict struct {
	Key            string
	Local          json.RawMessage
	Remote         json.RawMessage
	FieldConflicts []FieldConflict // Field-level conflicts within the server config
}

// MCPPushResult describes the outcome of pushing MCP configs.
type MCPPushResult struct {
	ServersPushed int
	Unchanged     bool
}

// MCPPullResult describes the outcome of pulling MCP configs.
type MCPPullResult struct {
	Added     []string
	Updated   []string
	Kept      []string
	Conflicts []MCPConflict
	NoRemote  bool
}

// ReadMCPServers reads the mcpServers key from a claude.json file.
// Returns nil (not an error) if the file doesn't exist or has no mcpServers key.
func ReadMCPServers(path string) (MCPServers, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}

	mcpRaw, ok := raw["mcpServers"]
	if !ok || len(mcpRaw) == 0 {
		return nil, nil
	}

	var servers MCPServers
	if err := json.Unmarshal(mcpRaw, &servers); err != nil {
		return nil, fmt.Errorf("failed to parse mcpServers: %w", err)
	}

	return servers, nil
}

// WriteMCPServers writes the mcpServers key into a claude.json file,
// preserving all other keys. Creates a .bak backup before writing.
func WriteMCPServers(path string, servers MCPServers) error {
	// Read existing file (or start with empty object)
	var raw map[string]json.RawMessage

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to read %s: %w", path, err)
		}
		raw = make(map[string]json.RawMessage)
	} else {
		// Create backup
		if err := os.WriteFile(path+".bak", data, 0600); err != nil {
			return fmt.Errorf("failed to create backup: %w", err)
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("failed to parse %s: %w", path, err)
		}
	}

	// Marshal the servers and set the key
	serversJSON, err := json.Marshal(servers)
	if err != nil {
		return fmt.Errorf("failed to serialize mcpServers: %w", err)
	}
	raw["mcpServers"] = serversJSON

	// Write back with indentation matching Claude Code's format
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize claude.json: %w", err)
	}

	if err := os.WriteFile(path, append(out, '\n'), 0600); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}

	return nil
}

// NormalizeMCPPaths replaces machine-specific absolute paths with portable
// variable references (e.g., /Users/alice -> ${HOME}) in the raw JSON bytes.
func NormalizeMCPPaths(data []byte, homeDir string) []byte {
	if homeDir == "" {
		return data
	}
	// Ensure no trailing slash for consistent replacement
	homeDir = strings.TrimRight(homeDir, "/")
	return bytes.ReplaceAll(data, []byte(homeDir), []byte("${HOME}"))
}

// ResolveMCPPaths replaces portable variable references with the local
// machine's paths (e.g., ${HOME} -> /Users/bob) in the raw JSON bytes.
func ResolveMCPPaths(data []byte, homeDir string) []byte {
	if homeDir == "" {
		return data
	}
	homeDir = strings.TrimRight(homeDir, "/")
	return bytes.ReplaceAll(data, []byte("${HOME}"), []byte(homeDir))
}

// NormalizeMCPServers normalizes all path references in the server configs.
func NormalizeMCPServers(servers MCPServers, homeDir string) (MCPServers, error) {
	if servers == nil {
		return nil, nil
	}
	data, err := json.Marshal(servers)
	if err != nil {
		return nil, err
	}
	normalized := NormalizeMCPPaths(data, homeDir)
	var result MCPServers
	if err := json.Unmarshal(normalized, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ResolveMCPServers resolves all variable references in the server configs.
func ResolveMCPServers(servers MCPServers, homeDir string) (MCPServers, error) {
	if servers == nil {
		return nil, nil
	}
	data, err := json.Marshal(servers)
	if err != nil {
		return nil, err
	}
	resolved := ResolveMCPPaths(data, homeDir)
	var result MCPServers
	if err := json.Unmarshal(resolved, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// canonicalJSON returns a canonical (sorted keys) JSON representation.
func canonicalJSON(data []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// mcpServerEqual compares two server configs by their canonical JSON representation.
// Key order is ignored - {"a":1,"b":2} equals {"b":2,"a":1}.
func mcpServerEqual(a, b json.RawMessage) bool {
	aCanon, err := canonicalJSON(a)
	if err != nil {
		return false
	}
	bCanon, err := canonicalJSON(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aCanon, bCanon)
}

// jsonEqual compares two arbitrary JSON values for equality.
// Key order is ignored for objects.
func jsonEqual(a, b any) bool {
	aJSON, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bJSON, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(aJSON, bJSON)
}

// serverMergeResult holds the result of merging a single server's config.
type serverMergeResult struct {
	merged    map[string]any
	conflicts []FieldConflict
	changed   bool // true if merged differs from local
}

// mergeServerConfigs performs field-level three-way merge of server configurations.
func mergeServerConfigs(local, remote, baseline json.RawMessage) serverMergeResult {
	var localMap, remoteMap, baselineMap map[string]any

	if err := json.Unmarshal(local, &localMap); err != nil {
		localMap = make(map[string]any)
	}
	if err := json.Unmarshal(remote, &remoteMap); err != nil {
		remoteMap = make(map[string]any)
	}
	if baseline != nil {
		if err := json.Unmarshal(baseline, &baselineMap); err != nil {
			baselineMap = make(map[string]any)
		}
	} else {
		baselineMap = make(map[string]any)
	}

	merged, conflicts := mergeObjects(localMap, remoteMap, baselineMap, "")
	changed := !jsonEqual(merged, localMap)

	return serverMergeResult{
		merged:    merged,
		conflicts: conflicts,
		changed:   changed,
	}
}

// mergeObjects recursively merges two maps with a baseline reference.
func mergeObjects(local, remote, baseline map[string]any, pathPrefix string) (map[string]any, []FieldConflict) {
	result := make(map[string]any)
	var conflicts []FieldConflict

	allKeys := make(map[string]bool)
	for k := range local {
		allKeys[k] = true
	}
	for k := range remote {
		allKeys[k] = true
	}
	for k := range baseline {
		allKeys[k] = true
	}

	for key := range allKeys {
		path := key
		if pathPrefix != "" {
			path = pathPrefix + "." + key
		}

		l, inLocal := local[key]
		r, inRemote := remote[key]
		b, inBaseline := baseline[key]

		switch {
		case !inLocal && inRemote && !inBaseline:
			result[key] = r

		case inLocal && !inRemote && !inBaseline:
			result[key] = l

		case inLocal && inRemote && inBaseline:
			localChanged := !jsonEqual(l, b)
			remoteChanged := !jsonEqual(r, b)

			switch {
			case !localChanged && !remoteChanged:
				result[key] = l
			case !localChanged && remoteChanged:
				result[key] = r
			case localChanged && !remoteChanged:
				result[key] = l
			default:
				if jsonEqual(l, r) {
					result[key] = l
				} else {
					merged, fieldConflicts := mergeValue(l, r, b, path)
					result[key] = merged
					conflicts = append(conflicts, fieldConflicts...)
				}
			}

		case inLocal && inRemote && !inBaseline:
			if jsonEqual(l, r) {
				result[key] = l
			} else {
				merged, fieldConflicts := mergeValue(l, r, nil, path)
				result[key] = merged
				conflicts = append(conflicts, fieldConflicts...)
			}

		case !inLocal && inRemote && inBaseline:
			if jsonEqual(r, b) {
				// local deleted, remote unchanged -> honor deletion
			} else {
				conflicts = append(conflicts, FieldConflict{Path: path, Remote: r})
			}

		case inLocal && !inRemote && inBaseline:
			if jsonEqual(l, b) {
				// remote deleted, local unchanged -> honor deletion
			} else {
				result[key] = l
				conflicts = append(conflicts, FieldConflict{Path: path, Local: l})
			}

		case !inLocal && !inRemote && inBaseline:
			// both deleted

		default:
			if inRemote {
				result[key] = r
			} else if inLocal {
				result[key] = l
			}
		}
	}

	return result, conflicts
}

// mergeValue attempts to merge two values, recursing into objects/arrays if possible.
func mergeValue(local, remote, baseline any, path string) (any, []FieldConflict) {
	localMap, localIsMap := local.(map[string]any)
	remoteMap, remoteIsMap := remote.(map[string]any)
	baselineMap, baselineIsMap := baseline.(map[string]any)

	if localIsMap && remoteIsMap {
		if !baselineIsMap {
			baselineMap = make(map[string]any)
		}
		return mergeObjects(localMap, remoteMap, baselineMap, path)
	}

	localArr, localIsArr := local.([]any)
	remoteArr, remoteIsArr := remote.([]any)

	if localIsArr && remoteIsArr {
		merged, conflicts := mergeArrays(localArr, remoteArr, baseline, path)
		return merged, conflicts
	}

	return local, []FieldConflict{{Path: path, Local: local, Remote: remote}}
}

// mergeArrays merges two arrays using three-way logic.
// For arrays with object elements that have identifiable keys, uses set semantics.
// For simple value arrays (strings, numbers), treats as ordered lists - conflict if both changed.
func mergeArrays(local, remote []any, baseline any, path string) ([]any, []FieldConflict) {
	baselineArr, hasBaseline := baseline.([]any)

	// Check if arrays contain objects (set semantics) or primitives (ordered list semantics)
	hasObjects := false
	for _, v := range local {
		if _, ok := v.(map[string]any); ok {
			hasObjects = true
			break
		}
	}
	if !hasObjects {
		for _, v := range remote {
			if _, ok := v.(map[string]any); ok {
				hasObjects = true
				break
			}
		}
	}

	// For primitive arrays (like args), use ordered list semantics
	if !hasObjects {
		localKey := jsonKey(local)
		remoteKey := jsonKey(remote)

		if localKey == remoteKey {
			return local, nil
		}

		if hasBaseline {
			baselineKey := jsonKey(baselineArr)
			localChanged := localKey != baselineKey
			remoteChanged := remoteKey != baselineKey

			switch {
			case !localChanged && !remoteChanged:
				return local, nil
			case !localChanged && remoteChanged:
				return remote, nil
			case localChanged && !remoteChanged:
				return local, nil
			default:
				// Both changed differently - conflict
				return local, []FieldConflict{{Path: path, Local: local, Remote: remote}}
			}
		}

		// No baseline, different values - conflict
		return local, []FieldConflict{{Path: path, Local: local, Remote: remote}}
	}

	// For object arrays, use set semantics with identifier detection
	localSet := make(map[string]any)
	remoteSet := make(map[string]any)
	baselineSet := make(map[string]any)

	for _, v := range local {
		key := objectIdentifier(v)
		localSet[key] = v
	}
	for _, v := range remote {
		key := objectIdentifier(v)
		remoteSet[key] = v
	}
	for _, v := range baselineArr {
		key := objectIdentifier(v)
		baselineSet[key] = v
	}

	var result []any
	seen := make(map[string]bool)

	for _, v := range local {
		key := objectIdentifier(v)
		if seen[key] {
			continue
		}
		seen[key] = true

		_, inRemote := remoteSet[key]
		_, inBaseline := baselineSet[key]

		if !inRemote && inBaseline {
			continue
		}
		result = append(result, v)
	}

	for _, v := range remote {
		key := objectIdentifier(v)
		if seen[key] {
			continue
		}
		seen[key] = true

		_, inLocal := localSet[key]
		_, inBaseline := baselineSet[key]

		if !inLocal && inBaseline {
			continue
		}
		result = append(result, v)
	}

	return result, nil
}

// objectIdentifier returns a stable identifier for an object.
// Tries common identifier fields, falls back to full JSON.
func objectIdentifier(v any) string {
	if m, ok := v.(map[string]any); ok {
		// Try common identifier fields
		for _, key := range []string{"name", "id", "command", "matcher"} {
			if id, exists := m[key]; exists {
				if s, ok := id.(string); ok && s != "" {
					return key + ":" + s
				}
			}
		}
	}
	return jsonKey(v)
}

// jsonKey returns a canonical string key for a JSON value (for set operations).
func jsonKey(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// MergeMCPServers performs a three-way merge of MCP server configurations.
// baseline is the last-synced state (may be nil for first sync).
func MergeMCPServers(local, remote, baseline MCPServers) *MCPMergeResult {
	result := &MCPMergeResult{
		Merged: make(MCPServers),
	}

	// Collect all keys across all three sets
	allKeys := make(map[string]bool)
	for k := range local {
		allKeys[k] = true
	}
	for k := range remote {
		allKeys[k] = true
	}
	for k := range baseline {
		allKeys[k] = true
	}

	for key := range allKeys {
		l, inLocal := local[key]
		r, inRemote := remote[key]
		b, inBaseline := baseline[key]

		switch {
		// Only in remote (new from another device)
		case !inLocal && inRemote && !inBaseline:
			result.Merged[key] = r
			result.Added = append(result.Added, key)

		// Only in local (not yet synced)
		case inLocal && !inRemote && !inBaseline:
			result.Merged[key] = l
			result.Kept = append(result.Kept, key)

		// In all three
		case inLocal && inRemote && inBaseline:
			localMatchesBaseline := mcpServerEqual(l, b)
			remoteMatchesBaseline := mcpServerEqual(r, b)

			switch {
			case localMatchesBaseline && remoteMatchesBaseline:
				// No changes
				result.Merged[key] = l
				result.Kept = append(result.Kept, key)
			case localMatchesBaseline && !remoteMatchesBaseline:
				// Only remote changed -> update from remote
				result.Merged[key] = r
				result.Updated = append(result.Updated, key)
			case !localMatchesBaseline && remoteMatchesBaseline:
				// Only local changed -> keep local
				result.Merged[key] = l
				result.Kept = append(result.Kept, key)
			default:
				// Both changed - attempt field-level merge
				if mcpServerEqual(l, r) {
					result.Merged[key] = l
					result.Kept = append(result.Kept, key)
				} else {
					mergeRes := mergeServerConfigs(l, r, b)
					mergedJSON, _ := json.Marshal(mergeRes.merged)
					result.Merged[key] = mergedJSON

					if len(mergeRes.conflicts) > 0 {
						result.Conflicts = append(result.Conflicts, MCPConflict{
							Key:            key,
							Local:          l,
							Remote:         r,
							FieldConflicts: mergeRes.conflicts,
						})
					}
					if mergeRes.changed {
						result.Updated = append(result.Updated, key)
					} else {
						result.Kept = append(result.Kept, key)
					}
				}
			}

		// In local and remote, no baseline (first sync)
		case inLocal && inRemote && !inBaseline:
			if mcpServerEqual(l, r) {
				result.Merged[key] = l
				result.Kept = append(result.Kept, key)
			} else {
				// Field-level merge without baseline
				mergeRes := mergeServerConfigs(l, r, nil)
				mergedJSON, _ := json.Marshal(mergeRes.merged)
				result.Merged[key] = mergedJSON

				if len(mergeRes.conflicts) > 0 {
					result.Conflicts = append(result.Conflicts, MCPConflict{
						Key:            key,
						Local:          l,
						Remote:         r,
						FieldConflicts: mergeRes.conflicts,
					})
				}
				if mergeRes.changed {
					result.Updated = append(result.Updated, key)
				} else {
					result.Kept = append(result.Kept, key)
				}
			}

		// In baseline and remote, deleted locally
		case !inLocal && inRemote && inBaseline:
			if mcpServerEqual(r, b) {
				// Remote unchanged, honor local deletion -> omit
			} else {
				// Remote changed after local deletion -> conflict
				result.Conflicts = append(result.Conflicts, MCPConflict{
					Key:    key,
					Remote: r,
				})
			}

		// In baseline and local, deleted remotely
		case inLocal && !inRemote && inBaseline:
			if mcpServerEqual(l, b) {
				// Local unchanged, honor remote deletion -> omit
			} else {
				// Local changed after remote deletion -> conflict
				result.Merged[key] = l
				result.Conflicts = append(result.Conflicts, MCPConflict{
					Key:   key,
					Local: l,
				})
			}

		// Only in baseline (deleted on both sides) -> omit
		case !inLocal && !inRemote && inBaseline:
			// Both deleted, nothing to do

		// Only in remote, was in baseline (shouldn't happen but handle gracefully)
		default:
			if inRemote {
				result.Merged[key] = r
				result.Added = append(result.Added, key)
			} else if inLocal {
				result.Merged[key] = l
				result.Kept = append(result.Kept, key)
			}
		}
	}

	return result
}

// HashMCPServers computes a hash of the normalized MCP servers for change detection.
func HashMCPServers(servers MCPServers) (string, error) {
	if servers == nil {
		return "", nil
	}
	data, err := json.Marshal(servers)
	if err != nil {
		return "", err
	}
	// Compact to canonical form
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return "", err
	}
	return hashBytes(buf.Bytes()), nil
}

func hashBytes(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
