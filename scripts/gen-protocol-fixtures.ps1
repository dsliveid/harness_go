# Regenerate the golden protocol fixtures consumed by hd-protocol's drift gate.
#
# Usage:
#   .\scripts\gen-protocol-fixtures.ps1
#
# Run this whenever you change:
#   - harness_go/internal/device/protocol.go (any struct or field)
#   - internal/model/device.go (Capability / protocol version constants)
#   - internal/apperr (fs.* or device.* error codes)
#
# CI fails if regenerating produces a non-empty git diff: that means someone
# changed the protocol without committing the fixtures, and the Rust side would
# silently drift out of sync.
#
# NOTE: ASCII-only on purpose. Windows PowerShell 5.1 reads BOM-less UTF-8 as
# ANSI, and multibyte CJK bytes break the parser.

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$out = Join-Path $root '..\harness_desktop\crates\hd-protocol\fixtures'
$parent = Split-Path -Parent $out

if (-not (Test-Path $parent)) {
    Write-Error "Target directory not found; expected harness_desktop beside harness_go: $out"
    exit 1
}

Push-Location $root
go run scripts/gen-fixtures.go -out $out
$code = $LASTEXITCODE
Pop-Location

if ($code -ne 0) {
    Write-Error "fixture generation failed (exit $code)"
    exit $code
}

Write-Host "fixtures synced to $out"
