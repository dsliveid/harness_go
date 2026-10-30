# B-2 / B-3 / B-4 end-to-end check on the real binary.
# Covers: auth(login/me/wrong-pwd/SSO 302), sessions CRUD, run start + SSE stream,
#         approval decide guard rails, providers CRUD + key redaction + connectivity test,
#         settings whitelist, stats, logs (tail + no secret leak), restart persistence.
# Usage: powershell -ExecutionPolicy Bypass -File dev/e2e-b2b4.ps1
# NOTE: keep this file pure ASCII (PS 5.1 mis-parses BOM-less UTF-8 CJK text).

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$bin = Join-Path $root 'bin/harness-go.exe'
$dataDir = Join-Path $root '.harness/tmp/b2b4-data'
$port = 18086
$base = "http://127.0.0.1:$port"
$user = 'admin'
$pwd = 'e2e-pass-123'
$failed = @()

function Add-Fail([string]$msg) { $script:failed += $msg }

function Get-StatusCode([scriptblock]$Action) {
    try { & $Action | Out-Null; return 200 }
    catch {
        if ($_.Exception.Response) { return [int]$_.Exception.Response.StatusCode }
        return -1
    }
}

function Wait-Ready {
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 250
        try {
            $h = Invoke-RestMethod -Uri "$base/healthz" -TimeoutSec 2
            if ($h.status -eq 'ok') { return $h }
        } catch { }
    }
    throw 'server not ready within 10s'
}

Write-Host '[1] build binary ...'
go build -trimpath -ldflags '-s -w' -o $bin ./cmd/harness-go
if ($LASTEXITCODE -ne 0) { throw 'build failed' }

Write-Host '[2] start server on clean data dir ...'
if (Test-Path $dataDir) { Remove-Item $dataDir -Recurse -Force }
$env:HARNESS_USER = $user
$env:HARNESS_PWD = $pwd
$proc = Start-Process -FilePath $bin -ArgumentList '-data', $dataDir, '-addr', "127.0.0.1:$port" -WindowStyle Hidden -PassThru
try {
    $h = Wait-Ready
    Write-Host ('    healthz ok, protocol_version=' + $h.protocol_version)

    # ---------------- B-2 auth ----------------
    Write-Host '[3] auth: login / me / wrong password ...'
    $login = Invoke-RestMethod -Uri "$base/api/v1/auth/login" -Method Post -ContentType 'application/json' -Body (@{ username = $user; password = $pwd } | ConvertTo-Json)
    if (-not $login.token) { Add-Fail 'login returned no token' }
    $H = @{ Authorization = 'Bearer ' + $login.token }
    $me = Invoke-RestMethod -Uri "$base/api/v1/auth/me" -Headers $H
    if ($me.user.username -ne $user) { Add-Fail ('me.username expected ' + $user + ', got ' + $me.user.username) }
    if ($me.user.role -ne 'admin' -and $me.user.role -ne 'owner') { Add-Fail ('me.role expected admin/owner, got ' + $me.user.role) }

    $badCode = Get-StatusCode { Invoke-RestMethod -Uri "$base/api/v1/auth/login" -Method Post -ContentType 'application/json' -Body '{"username":"admin","password":"wrong-pwd"}' }
    if ($badCode -ne 401) { Add-Fail ('wrong password expected 401, got ' + $badCode) }
    $noAuthCode = Get-StatusCode { Invoke-RestMethod -Uri "$base/api/v1/sessions" }
    if ($noAuthCode -ne 401) { Add-Fail ('unauthenticated sessions expected 401, got ' + $noAuthCode) }

    Write-Host '[4] auth: SSO entry must 302 and strip credentials from Location ...'
    $raw = & curl.exe -s -i --max-time 8 "$base/?user_name=$user&pwd=$pwd" 2>&1 | Out-String
    if ($raw -notmatch '302') { Add-Fail 'SSO entry did not return 302' }
    if ($raw -match 'pwd=') { Add-Fail 'SSO Location/headers still contain pwd' }
    if ($raw -notmatch 'hg_sess') { Add-Fail 'SSO did not set hg_sess cookie' }

    # ---------------- B-3 sessions / runs / approvals ----------------
    Write-Host '[5] sessions: create / list / rename / messages ...'
    $sess = Invoke-RestMethod -Uri "$base/api/v1/sessions" -Method Post -Headers $H -ContentType 'application/json' -Body '{"title":"e2e-b2b4"}'
    if (-not $sess.id) { throw 'session create returned no id' }
    $sid = $sess.id
    $list = Invoke-RestMethod -Uri "$base/api/v1/sessions" -Headers $H
    if (-not ($list.items | Where-Object { $_.id -eq $sid })) { Add-Fail 'created session missing from list' }
    Invoke-RestMethod -Uri "$base/api/v1/sessions/$sid" -Method Patch -Headers $H -ContentType 'application/json' -Body '{"title":"e2e-renamed"}' | Out-Null
    $one = Invoke-RestMethod -Uri "$base/api/v1/sessions/$sid" -Headers $H
    if ($one.title -ne 'e2e-renamed') { Add-Fail ('rename failed, title=' + $one.title) }
    $msgs = Invoke-RestMethod -Uri "$base/api/v1/sessions/$sid/messages" -Headers $H
    if ($null -eq $msgs.items) { Add-Fail 'messages items missing' }

    Write-Host '[6] run: start + SSE stream delivers events ...'
    # No real provider is configured on a clean data dir: the Run is expected to reach a
    # terminal state (failed) - what we assert here is that the run API + SSE plumbing works.
    $run = Invoke-RestMethod -Uri "$base/api/v1/sessions/$sid/runs" -Method Post -Headers $H -ContentType 'application/json' -Body '{"input":"hello e2e","model_ref":"e2e:gpt-test"}'
    if (-not $run.run_id) { throw 'run create returned no run_id' }
    $rid = $run.run_id
    $sse = & curl.exe -s -N --max-time 10 -H "Authorization: Bearer $($login.token)" "$base/api/v1/sessions/$sid/runs/$rid/events" 2>&1 | Out-String
    if ($sse -notmatch 'event:') { Add-Fail 'SSE stream produced no event lines' }
    if ($sse -notmatch 'run.status|error') { Add-Fail 'SSE stream produced no run.status/error event' }
    $runFinal = Invoke-RestMethod -Uri "$base/api/v1/sessions/$sid/runs/$rid" -Headers $H
    Write-Host ('    run final status=' + $runFinal.status + ' stop_reason=' + $runFinal.stop_reason)

    Write-Host '[7] approvals: guard rails (400 / 404 / 409) ...'
    $ghost = '00000000-0000-0000-0000-000000000000'
    $c400 = Get-StatusCode { Invoke-RestMethod -Uri "$base/api/v1/sessions/$sid/runs/$rid/approvals/$ghost/decide" -Method Post -Headers $H -ContentType 'application/json' -Body '{"decision":"bogus"}' }
    if ($c400 -ne 400) { Add-Fail ('invalid decision expected 400, got ' + $c400) }
    $cNotFound = Get-StatusCode { Invoke-RestMethod -Uri "$base/api/v1/sessions/$sid/runs/$ghost/approvals/$ghost/decide" -Method Post -Headers $H -ContentType 'application/json' -Body '{"decision":"allow_once"}' }
    if ($cNotFound -ne 404) { Add-Fail ('unknown run expected 404, got ' + $cNotFound) }
    $cNotWaiting = Get-StatusCode { Invoke-RestMethod -Uri "$base/api/v1/sessions/$sid/runs/$rid/approvals/$ghost/decide" -Method Post -Headers $H -ContentType 'application/json' -Body '{"decision":"allow_once"}' }
    if ($cNotWaiting -ne 409 -and $cNotWaiting -ne 404) { Add-Fail ('run not waiting_approval expected 409/404, got ' + $cNotWaiting) }

    # ---------------- B-4 providers / settings / stats / logs ----------------
    Write-Host '[8] providers: create (key redaction + base_url trim) ...'
    $pbody = @{ name = 'e2e-provider'; base_url = 'https://api.example.com/v1/'; api_key = 'sk-e2e-secret'; models = @('gpt-test') } | ConvertTo-Json
    $p = Invoke-RestMethod -Uri "$base/api/v1/providers" -Method Post -Headers $H -ContentType 'application/json' -Body $pbody
    if (-not $p.id) { throw 'provider create returned no id' }
    if ($p.api_key) { Add-Fail 'provider create leaked api_key in response' }
    if (-not $p.has_api_key) { Add-Fail 'provider create missing has_api_key=true' }
    if ($p.base_url -ne 'https://api.example.com/v1') { Add-Fail ('base_url not normalized: ' + $p.base_url) }
    $plist = Invoke-RestMethod -Uri "$base/api/v1/providers" -Headers $H
    if (-not ($plist.items | Where-Object { $_.id -eq $p.id })) { Add-Fail 'created provider missing from list' }
    if (($plist.items | Where-Object { $_.api_key }).Count -gt 0) { Add-Fail 'provider list leaked api_key' }

    Write-Host '[9] providers: update keeps key when api_key omitted ...'
    $upd = Invoke-RestMethod -Uri "$base/api/v1/providers/$($p.id)" -Method Patch -Headers $H -ContentType 'application/json' -Body '{"name":"e2e-provider-2","base_url":"https://api.example.com/v2","models":["gpt-test"]}'
    if ($upd.name -ne 'e2e-provider-2') { Add-Fail 'provider rename failed' }
    if (-not $upd.has_api_key) { Add-Fail 'provider update dropped api_key' }

    Write-Host '[10] providers: connectivity test (unreachable -> ok=false, HTTP 200) ...'
    $t = Invoke-RestMethod -Uri "$base/api/v1/providers/$($p.id)/test" -Method Post -Headers $H -ContentType 'application/json' -Body '{}'
    if ($t.ok -ne $false) { Add-Fail ('connectivity test expected ok=false, got ' + $t.ok) }
    if (-not $t.code) { Add-Fail 'connectivity test missing error code' }
    Write-Host ('    test ok=' + $t.ok + ' code=' + $t.code)

    Write-Host '[11] settings: get / patch whitelist / reject unknown key ...'
    $s0 = Invoke-RestMethod -Uri "$base/api/v1/settings" -Headers $H
    if ($null -eq $s0.items) { Add-Fail 'settings items missing' }
    $sp = Invoke-RestMethod -Uri "$base/api/v1/settings" -Method Patch -Headers $H -ContentType 'application/json' -Body '{"ui.theme":"dark","agent.max_steps":12}'
    if ($sp.items.'ui.theme' -ne 'dark') { Add-Fail 'settings patch did not persist ui.theme' }
    $cBadKey = Get-StatusCode { Invoke-RestMethod -Uri "$base/api/v1/settings" -Method Patch -Headers $H -ContentType 'application/json' -Body '{"evil.key":"x"}' }
    if ($cBadKey -ne 400) { Add-Fail ('unknown settings key expected 400, got ' + $cBadKey) }
    $cEmpty = Get-StatusCode { Invoke-RestMethod -Uri "$base/api/v1/settings" -Method Patch -Headers $H -ContentType 'application/json' -Body '{}' }
    if ($cEmpty -ne 400) { Add-Fail ('empty settings patch expected 400, got ' + $cEmpty) }

    Write-Host '[12] stats ...'
    $st = Invoke-RestMethod -Uri "$base/api/v1/stats" -Headers $H
    if (-not $st.protocol_version) { Add-Fail 'stats missing protocol_version' }
    if ($st.sessions -lt 1) { Add-Fail ('stats.sessions expected >=1, got ' + $st.sessions) }

    Write-Host '[13] logs: tail + level filter + no secret leak ...'
    $lg = Invoke-RestMethod -Uri "$base/api/v1/logs?limit=50" -Headers $H
    if ($null -eq $lg.items) { Add-Fail 'logs items missing' }
    if ($lg.items.Count -lt 1) { Add-Fail 'logs tail is empty' }
    $logText = ($lg.items | ForEach-Object { $_.text }) -join "`n"
    if ($logText -match [regex]::Escape('sk-e2e-secret')) { Add-Fail 'api key leaked into app.log' }
    if ($logText -match [regex]::Escape($pwd)) { Add-Fail 'password leaked into app.log' }
    Invoke-RestMethod -Uri "$base/api/v1/logs?limit=5&level=ERROR" -Headers $H | Out-Null

    Write-Host '[14] providers: delete -> 204, second delete -> 404 ...'
    Invoke-RestMethod -Uri "$base/api/v1/providers/$($p.id)" -Method Delete -Headers $H | Out-Null
    $cDup = Get-StatusCode { Invoke-RestMethod -Uri "$base/api/v1/providers/$($p.id)" -Method Delete -Headers $H }
    if ($cDup -ne 404) { Add-Fail ('second provider delete expected 404, got ' + $cDup) }

    Write-Host '[15] restart: settings must persist ...'
    Stop-Process -Id $proc.Id -Force
    Start-Sleep -Milliseconds 900
    $proc = Start-Process -FilePath $bin -ArgumentList '-data', $dataDir, '-addr', "127.0.0.1:$port" -WindowStyle Hidden -PassThru
    Wait-Ready | Out-Null
    $login2 = Invoke-RestMethod -Uri "$base/api/v1/auth/login" -Method Post -ContentType 'application/json' -Body (@{ username = $user; password = $pwd } | ConvertTo-Json)
    $H2 = @{ Authorization = 'Bearer ' + $login2.token }
    $s2 = Invoke-RestMethod -Uri "$base/api/v1/settings" -Headers $H2
    if ($s2.items.'ui.theme' -ne 'dark') { Add-Fail 'ui.theme lost after restart' }
    $sess2 = Invoke-RestMethod -Uri "$base/api/v1/sessions/$sid" -Headers $H2
    if ($sess2.title -ne 'e2e-renamed') { Add-Fail 'session lost after restart' }
}
finally {
    if ($proc -and -not $proc.HasExited) { Stop-Process -Id $proc.Id -Force }
}

Write-Host ''
if ($failed.Count -gt 0) {
    foreach ($f in $failed) { Write-Host ('FAIL: ' + $f) -ForegroundColor Red }
    exit 1
}
Write-Host 'OK: real-binary e2e passed (B-2 auth / B-3 sessions+runs+approvals / B-4 providers+settings+stats+logs)' -ForegroundColor Green
exit 0
