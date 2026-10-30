# A-10 end-to-end check on the real binary: tools endpoints + tier patching.
# Usage: powershell -ExecutionPolicy Bypass -File dev/e2e-a10.ps1
# NOTE: keep this file pure ASCII (PS 5.1 mis-parses BOM-less UTF-8 CJK text).

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$bin = Join-Path $root 'bin/harness-go.exe'
$dataDir = Join-Path $root '.harness/tmp/a10-data'
$base = 'http://127.0.0.1:18085'
$failed = @()

Write-Host '[1] build binary ...'
go build -trimpath -ldflags '-s -w' -o $bin ./cmd/harness-go
if ($LASTEXITCODE -ne 0) { throw 'build failed' }

Write-Host '[2] start server on clean data dir ...'
if (Test-Path $dataDir) { Remove-Item $dataDir -Recurse -Force }
$env:HARNESS_USER = 'admin'
$env:HARNESS_PWD = 'e2e-pass-123'
$proc = Start-Process -FilePath $bin -ArgumentList '-data', $dataDir, '-addr', '127.0.0.1:18085' -WindowStyle Hidden -PassThru
try {
    $ready = $false
    $h = $null
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 250
        try {
            $h = Invoke-RestMethod -Uri "$base/healthz" -TimeoutSec 2
            if ($h.status -eq 'ok') { $ready = $true; break }
        } catch { }
    }
    if (-not $ready) { throw 'server not ready within 10s' }
    Write-Host ('    healthz ok, protocol_version=' + $h.protocol_version)

    Write-Host '[3] login ...'
    $login = Invoke-RestMethod -Uri "$base/api/v1/auth/login" -Method Post -ContentType 'application/json' -Body '{"username":"admin","password":"e2e-pass-123"}'
    if (-not $login.token) { throw 'no token returned' }
    $headers = @{ Authorization = 'Bearer ' + $login.token }

    Write-Host '[4] GET /api/v1/tools ...'
    $snap = Invoke-RestMethod -Uri "$base/api/v1/tools" -Headers $headers
    if ($snap.total -lt 1) { $failed += 'tools snapshot is empty' }
    $echoTool = $snap.core | Where-Object { $_.name -eq 'echo' } | Select-Object -First 1
    if (-not $echoTool) {
        $failed += 'echo missing from core tier'
    } else {
        if ($echoTool.tier -ne 'core') { $failed += ('echo tier expected core, got ' + $echoTool.tier) }
        if ($echoTool.default_tier -ne 'core') { $failed += ('echo default_tier expected core, got ' + $echoTool.default_tier) }
        if ($echoTool.cost_tokens -le 0) { $failed += 'echo cost_tokens must be > 0' }
        if ($echoTool.risk -ne 'readonly') { $failed += ('echo risk expected readonly, got ' + $echoTool.risk) }
        if ($echoTool.health.status -ne 'available') { $failed += ('echo health expected available, got ' + $echoTool.health.status) }
        Write-Host ('    echo: tier=' + $echoTool.tier + ' cost=' + $echoTool.cost_tokens + ' risk=' + $echoTool.risk + ' health=' + $echoTool.health.status)
    }
    if ($snap.limits.core_soft -ne 8 -or $snap.limits.deferred_soft -ne 60) {
        $failed += 'soft limits echo mismatch'
    }
    if ($snap.plugins.Count -lt 1) { $failed += 'plugins list is empty' }

    Write-Host '[5] GET /api/v1/tools/injection ...'
    $inj = Invoke-RestMethod -Uri "$base/api/v1/tools/injection" -Headers $headers
    if ($inj.core.Count -lt 1) { $failed += 'injection core is empty' }
    if ($inj.cost_core -le 0) { $failed += 'cost_core must be > 0' }
    Write-Host ('    cost_core=' + $inj.cost_core + ' cost_index=' + $inj.cost_index + ' cost_total=' + $inj.cost_total)

    Write-Host '[6] PATCH /api/v1/tools/tiers (echo -> on_demand) ...'
    $patched = Invoke-RestMethod -Uri "$base/api/v1/tools/tiers" -Method Patch -Headers $headers -ContentType 'application/json' -Body '{"tiers":{"echo":"on_demand"}}'
    $onDemand = $patched.on_demand | Where-Object { $_.name -eq 'echo' } | Select-Object -First 1
    if (-not $onDemand) { $failed += 'echo did not move to on_demand' }
    if ($patched.overrides.echo -ne 'on_demand') { $failed += 'overrides.echo not recorded' }

    Write-Host '[7] re-check injection (core must be empty) ...'
    $inj2 = Invoke-RestMethod -Uri "$base/api/v1/tools/injection" -Headers $headers
    if ($inj2.core.Count -ne 0) { $failed += ('core should be empty after patch, got ' + $inj2.core.Count) }

    Write-Host '[8] invalid requests must be 400 ...'
    $badBodies = @('{"tiers":{"echo":"bogus"}}', '{"tiers":{"ghost":"core"}}', '{"tiers":{}}')
    foreach ($body in $badBodies) {
        $code = 0
        try {
            Invoke-RestMethod -Uri "$base/api/v1/tools/tiers" -Method Patch -Headers $headers -ContentType 'application/json' -Body $body | Out-Null
            $code = 200
        } catch {
            $code = [int]$_.Exception.Response.StatusCode
        }
        if ($code -ne 400) { $failed += ('request ' + $body + ' expected 400, got ' + $code) }
    }

    Write-Host '[9] tier override must persist across restart ...'
    Stop-Process -Id $proc.Id -Force
    Start-Sleep -Milliseconds 900
    $proc = Start-Process -FilePath $bin -ArgumentList '-data', $dataDir, '-addr', '127.0.0.1:18085' -WindowStyle Hidden -PassThru
    $ready = $false
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 250
        try {
            $h2 = Invoke-RestMethod -Uri "$base/healthz" -TimeoutSec 2
            if ($h2.status -eq 'ok') { $ready = $true; break }
        } catch { }
    }
    if (-not $ready) { throw 'server not ready after restart' }
    $login2 = Invoke-RestMethod -Uri "$base/api/v1/auth/login" -Method Post -ContentType 'application/json' -Body '{"username":"admin","password":"e2e-pass-123"}'
    $snap2 = Invoke-RestMethod -Uri "$base/api/v1/tools" -Headers @{ Authorization = 'Bearer ' + $login2.token }
    if ($snap2.overrides.echo -ne 'on_demand') { $failed += 'tier override lost after restart' }
}
finally {
    if ($proc -and -not $proc.HasExited) { Stop-Process -Id $proc.Id -Force }
}

Write-Host ''
if ($failed.Count -gt 0) {
    foreach ($f in $failed) { Write-Host ('FAIL: ' + $f) -ForegroundColor Red }
    exit 1
}
Write-Host 'OK: real-binary e2e passed (tools / injection / tiers / persistence)' -ForegroundColor Green
exit 0
