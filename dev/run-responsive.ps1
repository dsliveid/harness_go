# One-shot B-6 responsive regression runner.
#
# Boots the single binary on a throwaway data dir, runs the CDP three-tier viewport check
# (dev/responsive-check.mjs) against it, then shuts the server down.
#
# Usage: powershell -ExecutionPolicy Bypass -File dev/run-responsive.ps1
# Requires: bin/harness-go.exe (make build / go build) with web/dist already embedded,
#           Node >= 20.10 (global WebSocket via --experimental-websocket), a local Chrome/Edge.
#
# Env overrides: RESP_PORT (default 18090), RESP_SETTLE_MS (per-viewport settle time)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$port = if ($env:RESP_PORT) { $env:RESP_PORT } else { 18090 }
$exe = Join-Path $root 'bin\harness-go.exe'
$data = Join-Path $root '.harness\tmp\responsive-data'

if (-not (Test-Path $exe)) {
  Write-Output 'RESULT FAIL bin/harness-go.exe missing - run: go build -o bin/harness-go.exe ./cmd/harness-go'
  exit 1
}

# reuse the seeded credentials expected by the check script
$env:HARNESS_USER = 'admin'
$env:HARNESS_PWD = 'e2e-pass-123'

if (Test-Path $data) { Remove-Item -Recurse -Force $data -ErrorAction SilentlyContinue }
New-Item -ItemType Directory -Force -Path (Split-Path $data -Parent) | Out-Null

$proc = Start-Process -FilePath $exe -ArgumentList '-data', $data, '-addr', "127.0.0.1:$port" -PassThru -WindowStyle Hidden

$ready = $false
for ($i = 0; $i -lt 60; $i++) {
  try {
    $probe = Invoke-WebRequest -Uri "http://127.0.0.1:$port/healthz" -UseBasicParsing -TimeoutSec 2
    if ($probe.StatusCode -eq 200) { $ready = $true; break }
  } catch { }
  Start-Sleep -Milliseconds 300
}

if (-not $ready) {
  Write-Output 'RESULT FAIL server did not become healthy'
  Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
  exit 1
}

$env:RESP_BASE = "http://127.0.0.1:$port"
node --experimental-websocket dev/responsive-check.mjs
$code = $LASTEXITCODE

Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue

if ($code -eq 0) { Write-Output 'RESULT PASS' } else { Write-Output 'RESULT FAIL' }
exit $code
