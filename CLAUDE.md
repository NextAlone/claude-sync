# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test

```bash
make build        # go build -o bin/claude-sync ./cmd/claude-sync + codex-sync symlink
make test         # go test -v ./...
make check        # gofmt, go vet, go test ./... -short (pre-commit hook)
make fmt          # go fmt ./...
make lint         # golangci-lint run
make run          # build + run
make build-all    # cross-compile darwin/linux arm64/amd64
```

## Architecture

```
cmd/claude-sync/          # Entry point: cobra CLI (init/push/pull/status/diff/conflicts/reset/update/mcp)
internal/
  config/config.go        # YAML config load/save, sync paths, exclude globs, legacy R2 migration
  crypto/encrypt.go       # age X25519 encrypt/decrypt, passphrase→key via argon2+bech32
  storage/
    storage.go            # Storage interface (Upload/Download/Delete/List/Head/BucketExists) + factory
    config.go             # StorageConfig (provider/bucket/credentials), validate
    r2/r2.go              # Cloudflare R2 adapter (blank import registers via init())
    s3/s3.go              # AWS S3 adapter
    gcs/gcs.go            # GCS adapter
  sync/
    sync.go               # Syncer: Push/Pull/Diff/PreviewPull/Status, encrypted blobs, 10 concurrent workers
    state.go              # SyncState: file hashes + timestamps, tracks what's been pushed/pulled
    mcp.go                # MCP Push/Pull with field-level semantic merge of ~/.claude.json mcpServers
    codex_config.go       # Codex config.toml sanitization (strip local-only sections)
    settings_sync.go      # settings.json field-level sync: strip keys, canonical hash, three-way merge
  util/format.go          # FormatSize, TruncatePath, CompareVersions, GetBinaryName
```

**Dual binary**: Built as `claude-sync` by default; `binaryName == "codex-sync"` switches config dir to `~/.codex-sync`, source to `~/.codex`, activates Codex sync paths + excludes.

## Storage adapters

Each storage provider is a sub-package of `internal/storage/` that:
1. Exposes a `New(cfg *StorageConfig) (Storage, error)` func
2. Registers it by assigning to `storage.NewR2` / `storage.NewS3` / `storage.NewGCS` in `init()`
3. The blank imports in `cmd/claude-sync/main.go` and `sync/sync.go` trigger registration

## Testing

- Tests use `t.TempDir()` + `mockStorage` (in-memory map-based Storage), not real cloud calls
- Real `crypto.Encryptor` used (key derived from hardcoded passphrase)
- `Syncer` created via `NewSyncerWith()` — bypasses config file loading
- Add mock to `mockStorage` when new Storage methods are added

## Sync protocol

- Files gzipped then age-encrypted before upload
- Remote keys mirror local relative paths under `SourceDir`
- MCP configs stored at remote key `_external/mcp-servers.json`
- `.conflict.<timestamp>` files created when both sides changed
- Codex `config.toml` is sanitized pre-sync: `projects`/`marketplaces` blocks stripped

## Settings field-level sync

When `settings_sync` is configured in `config.yaml`, `settings.json` is synced with field-level merge instead of whole-file comparison:

```yaml
settings_sync:
  strip_keys:
    - env.ANTHROPIC_AUTH_TOKEN
    - env.ANTHROPIC_BASE_URL
```

- `strip_keys` — dot-separated JSON paths to strip before upload (e.g. `env.ANTHROPIC_AUTH_TOKEN`). Top-level keys (`env`) strip the entire sub-tree.
- Hash computed on canonical (sorted-key, stripped) JSON — key order irrelevant
- Push uploads stripped version; Pull uses three-way merge (local/remote/baseline) via same `mergeObjects` as MCP
- Stripped keys are restored from local after merge (never overwritten by remote)
- Baseline stored in `SyncState.SettingsBaseline`
