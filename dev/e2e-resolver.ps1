# Verify providers table is wired into the runtime model resolver (step 10).
# Expectation: a configured-but-unreachable provider must be actually used, so the run
# fails with a network error instead of returning the fake provider's echo reply.
# Usage: powershell -ExecutionPolicy Bypass -File dev/e2e-resolver.ps1

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$bin = Join-Path $root 'bin\harness-go.exe'
$dataDir = Join-Path $root '.harness/tmp/resolver-data'
$logFile = Join-Path $root '.harness/tmp/resolver.out.log'
$port = 18087
$base = "http://127.0.0.1:$port"
$env:HARNESS_USER = 'admin'
$env:HARNESS_PWD = 'e2e-pass-123'
$failed = @()

function Add-Fail([string]$m) { $script:failed += $m }

if (Test-Path $dataDir) { Remove-Item -Recurse -Force $dataDir }
go build -trimpath -o $bin ./cmd/harness-go
if ($LASTEXITCODE -ne 0) { throw 'build failed' }

Start-Process -FilePath $bin -ArgumentList '-data', $dataDir, '-addr', "127.0.0.1:$port" -RedirectStandardOutput $logFile -RedirectStandardError (Join-Path $root '.harness/tmp/resolver.err.log') -WindowStyle Hidden
for ($i = 0; $i -lt 40; $i++) {
  Start-Sleep -Milliseconds 250
  try { if ((Invoke-RestMethod -Uri "$base/healthz" -TimeoutSec 2).status -eq 'ok') { break } } catch { }
}

$login = Invoke-RestMethod -Uri "$base/api/v1/auth/login" -Method Post -ContentType 'application/json' -Body (@{ username = 'admin'; password = 'e2e-pass-123' } | ConvertTo-Json)
$H = @{ Authorization = 'Bearer ' + $login.token }

# providers: one enabled provider with an unreachable endpoint
$pvBody = @{ name = 'e2e-unreachable'; base_url = 'http://127.0.0.1:9/v1'; api_key = 'sk-test'; models = @('resolver-probe'); enabled = $true } | ConvertTo-Json
$pv = Invoke-RestMethod -Uri "$base/api/v1/providers" -Method Post -Headers $H -ContentType 'application/json' -Body $pvBody
if (-not $pv.id) { Add-Fail 'provider create returned no id' } else { Write-Host ("provider id=" + $pv.id) }

$sess = Invoke-RestMethod -Uri "$base/api/v1/sessions" -Method Post -Headers $H -ContentType 'application/json' -Body (@{ title = 'resolver probe' } | ConvertTo-Json)
Write-Host ("session id=" + $sess.id)

$runBody = @{ input = 'hi'; model_ref = 'resolver-probe' } | ConvertTo-Json
$run = Invoke-RestMethod -Uri "$base/api/v1/sessions/$($sess.id)/runs" -Method Post -Headers $H -ContentType 'application/json' -Body $runBody
Write-Host ("run id=" + $run.run_id + " status=" + $run.status)

Start-Sleep -Seconds 3
$final = Invoke-RestMethod -Uri "$base/api/v1/sessions/$($sess.id)/runs/$($run.run_id)" -Headers $H
Write-Host ("final status=" + $final.status + " error=" + $final.error_code + " msg=" + $final.error_message)
if ($final.status -ne 'failed') { Add-Fail ('expected failed run (unreachable provider), got ' + $final.status) }

$msgs = Invoke-RestMethod -Uri "$base/api/v1/sessions/$($sess.id)/messages" -Headers $H
$joined = ($msgs.items | ForEach-Object { $_.content }) -join ' '
if ($joined -match '\[fake\]') { Add-Fail 'assistant reply came from fake provider: providers table NOT used' }

Start-Sleep -Milliseconds 300
$hit = Select-String -Path $logFile -Pattern 'providers' -SimpleMatch
if (-not $hit) { Add-Fail 'log has no provider-resolution entry' } else { Write-Host 'log: provider resolution recorded' }

$line = netstat -ano | findstr ":$port" | Select-String 'LISTENING' | Select-Object -First 1
if ($line) { $procId = ($line -split '\s+')[-1]; taskkill /PID $procId /F 2>&1 | Out-Null }

if ($failed.Count -gt 0) {
  Write-Host 'FAILURES:'
  $failed | ForEach-Object { Write-Host (' - ' + $_) }
  exit 1
}
Write-Host 'OK: providers table is wired into the runtime resolver'
