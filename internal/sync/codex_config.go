package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

var codexLocalConfigTables = map[string]bool{
	"projects":     true,
	"marketplaces": true,
}

type tomlBlock struct {
	name  string
	local bool
	lines []string
}

func sanitizeCodexConfig(data []byte) []byte {
	blocks := splitTomlBlocks(data)
	var out []string
	for _, block := range blocks {
		if block.local {
			continue
		}
		out = append(out, block.lines...)
	}
	return normalizeTomlLines(out)
}

func mergeCodexConfig(local, remote []byte) []byte {
	localBlocks := splitTomlBlocks(local)
	remoteBlocks := splitTomlBlocks(remote)

	var out []string
	for _, block := range remoteBlocks {
		if block.local {
			continue
		}
		out = append(out, block.lines...)
	}
	for _, block := range localBlocks {
		if !block.local {
			continue
		}
		out = append(out, block.lines...)
	}
	return normalizeTomlLines(out)
}

func hashCodexConfig(data []byte) string {
	sum := sha256.Sum256(sanitizeCodexConfig(data))
	return hex.EncodeToString(sum[:])
}

func splitTomlBlocks(data []byte) []tomlBlock {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	blocks := []tomlBlock{{}}
	for _, line := range lines {
		if name, ok := tomlSectionName(line); ok {
			blocks = append(blocks, tomlBlock{
				name:  name,
				local: codexLocalConfigTables[firstTomlSegment(name)],
			})
		}
		blocks[len(blocks)-1].lines = append(blocks[len(blocks)-1].lines, line)
	}
	return blocks
}

func tomlSectionName(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "[[") && strings.HasSuffix(trimmed, "]]") {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "[["), "]]")), true
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")), true
	}
	return "", false
}

func firstTomlSegment(name string) string {
	inQuote := false
	escaped := false
	for i, r := range name {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && inQuote {
			escaped = true
			continue
		}
		if r == '"' {
			inQuote = !inQuote
			continue
		}
		if r == '.' && !inQuote {
			return strings.Trim(strings.TrimSpace(name[:i]), `"`)
		}
	}
	return strings.Trim(strings.TrimSpace(name), `"`)
}

func normalizeTomlLines(lines []string) []byte {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	var buf bytes.Buffer
	for i, line := range lines {
		if i > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(line)
	}
	buf.WriteByte('\n')
	return buf.Bytes()
}
