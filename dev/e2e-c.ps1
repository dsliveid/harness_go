# Stage C exit e2e check on the REAL binary + the REAL cmd/fake-runner executable.
#
# Usage: powershell -ExecutionPolicy Bypass -File dev/e2e-c.ps1
# Output ends with a single line: E2E_C_RESULT PASS|FAIL
#
# Scope (what only a real binary can prove -- keep it that way):
#   C-2  pair-code TTL 120s / single-use / anti-enumeration (identical error for consumed vs unknown)
#   C-3  device_token auth BEFORE upgrade (401, never 101) / welcome params / online heartbeat persisted
#   C-6  session lease endpoint shape (no lease == writable by server, epoch 0 -- NOT 404)
#   C-5  invariant 5: with the runner absent the Run still completes with NO user visible error
#   C-3  network pull + reconnect: runner killed -> device offline; reconnects with the same token -> same
#        device_id, online again (no re-pairing, no duplicate device row)
#   C-2  revoke -> 204, live runner receives device.revoked, reused token can no longer upgrade
#   log  redaction: no "hgd_" device token, no user password in the server log
#
# NOT here (already covered by in-process tests against a real WS gateway):
#   pair -> connect -> task.dispatch -> task.progress -> task.result -> degrade -> reconnect chain:
#     internal/device/gateway_chain_test.go (TestGatewayFullChainPairDispatchDegradeReconnect)
#   dispatch idempotency / deadline / offline-immediate-degrade: internal/device/dispatcher_test.go
#   Reason: no builtin tool is device-executable yet (echo is executor=server), and the offline
#   fake LLM provider emits no tool_calls, so a real-binary Run cannot trigger a task.dispatch.
#
# NOTE: keep this file pure ASCII + BOM-free (PS 5.1 mis-parses CJK); do NOT use curl.exe
#       (PowerShell strips JSON double quotes when passing args to native binaries).

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$port = if ($env:E2E_C_PORT) { $env:E2E_C_PORT } else { '18095' }
$dataDir = '.harness/tmp/e2e-c-data'
$logFile = '.harness/tmp/e2e-c.out.log'
$runnerLog = '.harness/tmp/e2e-c-runner.log'
$tokenFile = '.harness/tmp/e2e-c-device-token'
$exe = '.harness/tmp/harness-go-c.exe'
$runnerExe = '.harness/tmp/fake-runner-c.exe'
$pwd = 'e2e-c-pass-123'

New-Item -ItemType Directory -Force -Path '.harness/tmp' | Out-Null
Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue
foreach ($f in @($logFile, $runnerLog, $tokenFile)) { Remove-Item -Force $f -ErrorAction SilentlyContinue }
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

Write-Host '== build binaries (server + fake runner) =='
go build -o $exe ./cmd/harness-go
if ($LASTEXITCODE -ne 0) { Write-Host 'E2E_C_RESULT FAIL'; exit 1 }
go build -o $runnerExe ./cmd/fake-runner
if ($LASTEXITCODE -ne 0) { Write-Host 'E2E_C_RESULT FAIL'; exit 1 }

$env:HARNESS_USER = 'admin'
$env:HARNESS_PWD = $pwd

Write-Host "== start server on port $port =="
$launcher = "& '" + $exe + "' -data '" + $dataDir + "' -addr '127.0.0.1:" + $port + "' *> '" + $logFile + "'"
$proc = Start-Process -FilePath 'powershell' -ArgumentList '-NoProfile', '-NonInteractive', '-Command', $launcher -PassThru -WindowStyle Hidden
Start-Sleep -Seconds 3

$base = "http://127.0.0.1:$port"
$script:fail = 0
$runnerProc = $null

function Check($name, $cond, $detail) {
  if ($cond) { Write-Host "  PASS  $name" }
  else { Write-Host "  FAIL  $name  -> $detail"; $script:fail++ }
}

function CallApi($method, $path, $body, $token) {
  $headers = @{}
  if ($token) { $headers['Authorization'] = "Bearer $token" }
  $params = @{ Uri = "$base$path"; Method = $method; Headers = $headers; UseBasicParsing = $true }
  if ($null -ne $body) {
    $params['ContentType'] = 'application/json'
    $params['Body'] = [System.Text.Encoding]::UTF8.GetBytes(($body | ConvertTo-Json -Depth 6 -Compress))
  }
  try {
    $r = Invoke-WebRequest @params
    $content = [System.Text.Encoding]::UTF8.GetString($r.RawContentStream.ToArray())
    $parsed = $null
    if ($content) { try { $parsed = $content | ConvertFrom-Json } catch { $parsed = $null } }
    return @{ status = [int]$r.StatusCode; raw = $content; body = $parsed }
  }
  catch {
    $content = $_.ErrorDetails.Message
    if (-not $content) {
      $resp = $_.Exception.Response
      if ($null -eq $resp) { throw }
      $stream = $resp.GetResponseStream()
      $reader = New-Object System.IO.StreamReader($stream, [System.Text.Encoding]::UTF8)
      $content = $reader.ReadToEnd()
      $reader.Close()
    }
    $parsed = $null
    if ($content) { try { $parsed = $content | ConvertFrom-Json } catch { $parsed = $null } }
    return @{ status = [int]$_.Exception.Response.StatusCode; raw = $content; body = $parsed }
  }
}

# Start the fake runner in the background; returns the launcher process.
function StartRunner($argLine, $outFile) {
  $cmd = "& '" + $runnerExe + "' " + $argLine + " *> '" + $outFile + "'"
  return Start-Process -FilePath 'powershell' -ArgumentList '-NoProfile', '-NonInteractive', '-Command', $cmd -PassThru -WindowStyle Hidden
}

function WaitLog($file, $pattern, $timeoutMs) {
  $deadline = (Get-Date).AddMilliseconds($timeoutMs)
  while ((Get-Date) -lt $deadline) {
    if (Test-Path $file) {
      $txt = Get-Content -Raw -Encoding UTF8 $file -ErrorAction SilentlyContinue
      if ($txt -match $pattern) { return $txt }
    }
    Start-Sleep -Milliseconds 200
  }
  if (Test-Path $file) { return (Get-Content -Raw -Encoding UTF8 $file) }
  return ''
}

# Same device id used twice must never create a second row: assert the list contains exactly one entry.
function GetDeviceList($token) {
  return (CallApi 'Get' '/api/v1/devices' $null $token).body
}

try {
  # ---------- 0. login ----------
  $login = CallApi 'Post' '/api/v1/auth/login' @{ username = 'admin'; password = $pwd } $null
  Check 'login returns user token' ($login.status -eq 200 -and $login.body.token) $login.raw
  $token = $login.body.token

  # ---------- 1. pair code: TTL 120s + single use + anti-enumeration ----------
  $pc = CallApi 'Post' '/api/v1/devices/pair-codes' @{ name = 'C9 desktop'; kind = 'desktop' } $token
  Check 'pair-codes -> 201' ($pc.status -eq 201) $pc.raw
  Check 'pair code is 6 digits' ($pc.body.code -match '^\d{6}$') $pc.body.code
  Check 'pair code ttl_s = 120' ($pc.body.ttl_s -eq 120) $pc.body.ttl_s
  $code = $pc.body.code

  # ---------- 2. device auth before upgrade: 401 not 101 ----------
  $wsNoTok = CallApi 'Get' '/api/v1/devices/ws' $null $null
  Check 'ws without token -> 401 (never 101)' ($wsNoTok.status -eq 401) $wsNoTok.status
  $wsBadTok = CallApi 'Get' '/api/v1/devices/ws?token=hgd_deadbeef' $null $null
  Check 'ws with bad token -> 401' ($wsBadTok.status -eq 401) $wsBadTok.status

  # ---------- 3. fake runner pairs + connects ----------
  $runnerProc = StartRunner ("-server '" + $base + "' -code " + $code + " -token-file '" + $tokenFile + "' -name 'C9 runner' -task-delay 100ms -max-retries 3") $runnerLog
  $runnerText = WaitLog $runnerLog 'FAKE_RUNNER_READY' 25000
  Check 'fake runner paired' ($runnerText -match 'FAKE_RUNNER_PAIRED') $runnerText
  Check 'fake runner got welcome and is ready' ($runnerText -match 'FAKE_RUNNER_READY') $runnerText
  Check 'welcome heartbeat_s = 15' ($runnerText -match 'heartbeat_s=15') $runnerText
  Check 'welcome offline_after_s = 45' ($runnerText -match 'offline_after_s=45') $runnerText
  Check 'welcome server protocol v = 1' ($runnerText -match 'server_v=1') $runnerText

  # ---------- 4. pair code is single use; consumed vs unknown indistinguishable ----------
  $reuse = CallApi 'Post' '/api/v1/devices/pair' @{ code = $code; name = 'dup'; kind = 'desktop' } $null
  $ghost = CallApi 'Post' '/api/v1/devices/pair' @{ code = '000000'; name = 'ghost'; kind = 'desktop' } $null
  Check 'consumed pair code rejected with 401' ($reuse.status -eq 401) $reuse.status
  Check 'unknown pair code rejected with 401' ($ghost.status -eq 401) $ghost.status
  $reuseCode = if ($reuse.body -and $reuse.body.error) { $reuse.body.error.code } else { 'none' }
  $ghostCode = if ($ghost.body -and $ghost.body.error) { $ghost.body.error.code } else { 'none' }
  Check 'consumed vs unknown share one error code (anti-enumeration)' ($reuseCode -eq $ghostCode -and $reuseCode -ne 'none') ("$reuseCode vs $ghostCode")

  # ---------- 5. device token persisted once + device online ----------
  Start-Sleep -Milliseconds 600
  Check 'device token written to file (plaintext shown once)' (Test-Path $tokenFile) 'token file missing'
  $savedToken = if (Test-Path $tokenFile) { (Get-Content -Raw -Encoding UTF8 $tokenFile).Trim() } else { '' }
  Check 'device token has hgd_ prefix' ($savedToken -match '^hgd_') $savedToken

  $list1 = GetDeviceList $token
  $dev1 = if ($list1 -and $list1.items -and $list1.items.Count -gt 0) { $list1.items[0] } else { $null }
  Check 'device list has exactly 1 device' ($list1.items.Count -eq 1) "count=$($list1.items.Count)"
  Check 'device status online (heartbeat persisted)' ($null -ne $dev1 -and $dev1.status -eq 'online') $(if ($dev1) { $dev1.status } else { 'missing' })
  Check 'capabilities_hash persisted' ($null -ne $dev1 -and $dev1.capabilities_hash -ne '') $(if ($dev1) { $dev1.capabilities_hash } else { 'missing' })
  Check 'last_seen_at refreshed' ($null -ne $dev1 -and $null -ne $dev1.last_seen_at) 'last_seen_at empty'
  $deviceId = if ($dev1) { $dev1.id } else { '' }

  # ---------- 6. session lease endpoint shape (C-6) ----------
  $sess = CallApi 'Post' '/api/v1/sessions' @{ title = 'C9 e2e' } $token
  Check 'create session -> 201' ($sess.status -eq 201) $sess.raw
  $sid = $sess.body.id
  $lease = CallApi 'Get' "/api/v1/sessions/$sid/lease" $null $token
  Check 'lease endpoint -> 200 (absent lease is not 404)' ($lease.status -eq 200) $lease.status
  Check 'lease epoch = 0 when no cross-device writer' ($lease.body.epoch -eq 0) $lease.body.epoch
  Check 'lease holder_kind = server' ($lease.body.holder_kind -eq 'server') $lease.body.holder_kind
  Check 'lease active = true (server may write)' ($lease.body.active -eq $true) $lease.body.active

  # ---------- 7. network pull: runner killed -> device offline ----------
  taskkill /PID $runnerProc.Id /T /F 2>&1 | Out-Null
  Start-Sleep -Milliseconds 900
  $listOff = GetDeviceList $token
  $devOff = if ($listOff -and $listOff.items -and $listOff.items.Count -gt 0) { $listOff.items[0] } else { $null }
  Check 'device offline right after disconnect (no 45s wait)' ($null -ne $devOff -and $devOff.status -eq 'offline') $(if ($devOff) { $devOff.status } else { 'missing' })

  # ---------- 8. invariant 5: no device -> Run still completes, no user visible error ----------
  $run = CallApi 'Post' "/api/v1/sessions/$sid/runs" @{ input = 'hello c9'; model_ref = '' } $token
  Check 'run accepted while device absent' ($run.status -eq 201 -or $run.status -eq 202) $run.raw
  $runId = $run.body.run_id
  $final = $null
  for ($i = 0; $i -lt 40; $i++) {
    Start-Sleep -Milliseconds 250
    $final = (CallApi 'Get' "/api/v1/sessions/$sid/runs/$runId" $null $token).body
    if ($final -and $final.status -and $final.status -notin @('pending', 'running', 'waiting_approval')) { break }
  }
  Check 'run finished without a device (silent degrade, no error)' ($null -ne $final -and $final.status -eq 'completed') $(if ($final) { $final.status } else { 'no run' })
  Check 'no error surfaced to the user' ($null -eq $final -or $null -eq $final.error -or $final.error -eq '') $(if ($final) { $final.error } else { '' })
  $toolEvents = (CallApi 'Get' "/api/v1/sessions/$sid/runs/$runId/tool-events" $null $token).body
  Check 'no device-executed tool event recorded' ($null -eq $toolEvents.items -or $toolEvents.items.Count -eq 0) ("tool_events=" + $toolEvents.items.Count)

  # ---------- 9. reconnect with the same token: same device, online again ----------
  Remove-Item -Force $runnerLog -ErrorAction SilentlyContinue
  $runnerProc = StartRunner ("-server '" + $base + "' -token '" + $savedToken + "' -task-delay 100ms -max-retries 3") $runnerLog
  $recon = WaitLog $runnerLog 'FAKE_RUNNER_READY' 25000
  Check 'runner reconnects with the stored token (no re-pair)' ($recon -match 'FAKE_RUNNER_READY') $recon
  Check 'reconnect did not trigger a second pairing' (-not ($recon -match 'FAKE_RUNNER_PAIRED')) $recon
  $listRe = GetDeviceList $token
  Check 'still exactly 1 device after reconnect' ($listRe.items.Count -eq 1) "count=$($listRe.items.Count)"
  Check 'device_id unchanged after reconnect' ($listRe.items[0].id -eq $deviceId) ("$($listRe.items[0].id) vs $deviceId")
  Check 'device online again after reconnect' ($listRe.items[0].status -eq 'online') $listRe.items[0].status

  # ---------- 10. revoke: 204 -> runner told -> token can no longer upgrade ----------
  $revoke = CallApi 'Delete' "/api/v1/devices/$deviceId" $null $token
  Check 'revoke device -> 204' ($revoke.status -eq 204) $revoke.status
  $revokedLog = WaitLog $runnerLog 'device.revoked' 12000
  Check 'runner receives device.revoked' ($revokedLog -match 'device.revoked') $revokedLog
  Start-Sleep -Milliseconds 300
  $revoked = GetDeviceList $token
  Check 'device status revoked' ($revoked.items[0].status -eq 'revoked') $revoked.items[0].status
  $wsRevoked = CallApi 'Get' ("/api/v1/devices/ws?token=" + $savedToken) $null $null
  Check 'revoked token cannot upgrade -> 401' ($wsRevoked.status -eq 401) $wsRevoked.status

  # ---------- 11. log redaction ----------
  Start-Sleep -Milliseconds 400
  $logText = if (Test-Path $logFile) { Get-Content -Raw -Encoding UTF8 $logFile } else { '' }
  Check 'server log has access lines (server really ran)' ($logText -match 'access') 'no access line'
  Check 'server log has no device token plaintext' (-not ($logText -match 'hgd_')) 'device token leaked'
  Check 'server log has no user password' (-not ($logText -match $pwd)) 'password leaked'
  $accessLines = ($logText -split "`n") | Where-Object { $_ -match 'msg=access' }
  $leaky = $accessLines | Where-Object { $_ -match 'token' }
  Check 'access lines never contain a token key' ($null -eq $leaky) ($leaky -join ' | ')
}
finally {
  if ($runnerProc -and -not $runnerProc.HasExited) { taskkill /PID $runnerProc.Id /T /F 2>&1 | Out-Null }
  if ($proc -and -not $proc.HasExited) { taskkill /PID $proc.Id /T /F 2>&1 | Out-Null }
}

if ($script:fail -eq 0) { Write-Host 'E2E_C_RESULT PASS' }
else { Write-Host "E2E_C_RESULT FAIL ($script:fail checks failed)" }
if ($script:fail -ne 0) { exit 1 }
