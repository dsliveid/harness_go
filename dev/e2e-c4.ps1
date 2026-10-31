# 阶段 C-4 端到端验收：任务派发器 + 假 Runner（配对 → WS 连接 → welcome → 心跳 → 在线落库 → 令牌复用重连）
#
# 用法：powershell -ExecutionPolicy Bypass -File dev/e2e-c4.ps1
# 输出末尾固定为 E2E_C4_RESULT PASS|FAIL，便于上层断言。
#
# 说明：
#   1) 派发三帧闭环（task.dispatch/progress/result）与幂等、deadline 超时、离线降级
#      由 internal/device/dispatcher_test.go 用**真实 WS 网关**逐条断言（无需外部脚本）。
#   2) 本脚本只验证「真实二进制 + 真 cmd/fake-runner 可执行」的联调面：
#      配对发码 → 假 Runner 凭码配对 → 长连接 → welcome 参数 → 心跳 → 设备 online →
#      令牌落文件复用重连（不重复建设备）→ 错误令牌快速失败（不无限重试）。
#   3) 统一 Invoke-WebRequest（PowerShell 给原生程序传参会剥离 JSON 双引号，勿用 curl.exe）。
#   4) 服务输出经子进程 `*>` 重定向到文件，避免 run_command 因句柄继承挂起；收尾 taskkill /T。

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$port = if ($env:E2E_C4_PORT) { $env:E2E_C4_PORT } else { '18094' }
$dataDir = '.harness/tmp/e2e-c4-data'
$logFile = '.harness/tmp/e2e-c4.out.log'
$runnerLog = '.harness/tmp/e2e-c4-runner.log'
$tokenFile = '.harness/tmp/e2e-c4-device-token'
$exe = '.harness/tmp/harness-go-c4.exe'
$runnerExe = '.harness/tmp/fake-runner-c4.exe'

New-Item -ItemType Directory -Force -Path '.harness/tmp' | Out-Null
Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue
foreach ($f in @($logFile, $runnerLog, $tokenFile)) { Remove-Item -Force $f -ErrorAction SilentlyContinue }
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

Write-Host '== 构建二进制（服务端 + 假 Runner）=='
go build -o $exe ./cmd/harness-go
if ($LASTEXITCODE -ne 0) { Write-Host 'E2E_C4_RESULT FAIL'; exit 1 }
go build -o $runnerExe ./cmd/fake-runner
if ($LASTEXITCODE -ne 0) { Write-Host 'E2E_C4_RESULT FAIL'; exit 1 }

$env:HARNESS_USER = 'admin'
$env:HARNESS_PWD = 'e2e-c4-pass-123'

Write-Host "== 启动服务（端口 $port）=="
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
    return @{ status = [int]$r.StatusCode; raw = $content; body = ($content | ConvertFrom-Json) }
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
    if ($content) { $parsed = $content | ConvertFrom-Json }
    return @{ status = [int]$_.Exception.Response.StatusCode; raw = $content; body = $parsed }
  }
}

# 起假 Runner（后台），返回进程对象
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

try {
  # ① 登录取用户令牌
  $login = CallApi 'Post' '/api/v1/auth/login' @{ username = 'admin'; password = 'e2e-c4-pass-123' } $null
  Check '登录成功' ($login.status -eq 200 -and $login.body.token) "status=$($login.status)"
  $token = $login.body.token

  # ② 生成配对码（TTL 120s）
  $pc = CallApi 'Post' '/api/v1/devices/pair-codes' @{ name = '假 Runner 设备'; kind = 'desktop' } $token
  Check '生成配对码 201' ($pc.status -eq 201) $pc.raw
  Check '配对码为 6 位' ($pc.body.code.Length -eq 6) $pc.body.code
  Check '配对码 TTL = 120s' ($pc.body.ttl_s -eq 120) $pc.body.ttl_s
  $code = $pc.body.code

  # ③ 无效配对码：假 Runner 应快速失败（不得成功）
  $badProc = StartRunner ("-server '" + $base + "' -code 000000 -max-retries 0") '.harness/tmp/e2e-c4-badcode.log'
  $badDone = $badProc.WaitForExit(15000)
  $badLog = if (Test-Path '.harness/tmp/e2e-c4-badcode.log') { Get-Content -Raw -Encoding UTF8 '.harness/tmp/e2e-c4-badcode.log' } else { '' }
  Check '无效配对码时假 Runner 退出' ($badDone) '仍在运行'
  Check '无效配对码报错含 pair_code_invalid' ($badLog -match 'pair_code_invalid') $badLog
  if (-not $badProc.HasExited) { taskkill /PID $badProc.Id /T /F 2>&1 | Out-Null }
  Remove-Item -Force '.harness/tmp/e2e-c4-badcode.log' -ErrorAction SilentlyContinue

  # ④ 假 Runner 凭码配对 + 长连接 + welcome + 心跳
  $runnerProc = StartRunner ("-server '" + $base + "' -code " + $code + " -token-file '" + $tokenFile + "' -name '假 Runner 设备' -task-delay 100ms -max-retries 2") $runnerLog
  $runnerText = WaitLog $runnerLog 'FAKE_RUNNER_READY' 25000
  Check '假 Runner 配对成功' ($runnerText -match 'FAKE_RUNNER_PAIRED') $runnerText
  Check '假 Runner 收到 welcome 并就绪' ($runnerText -match 'FAKE_RUNNER_READY') $runnerText
  Check 'welcome 下发心跳间隔 15s' ($runnerText -match 'heartbeat_s=15') $runnerText
  Check 'welcome 下离线阈值 45s' ($runnerText -match 'offline_after_s=45') $runnerText
  Check 'welcome 下发协议版本 1' ($runnerText -match 'server_v=1') $runnerText

  # ⑤ 设备令牌落文件（配对成功仅此一次明文）+ 设备列表 online
  Start-Sleep -Milliseconds 600
  Check '设备令牌已写入文件' (Test-Path $tokenFile) '令牌文件不存在'
  $savedToken = if (Test-Path $tokenFile) { (Get-Content -Raw -Encoding UTF8 $tokenFile).Trim() } else { '' }
  Check '令牌形如 hgd_ 前缀' ($savedToken -match '^hgd_') $savedToken

  $list = (CallApi 'Get' '/api/v1/devices' $null $token).body
  $first = if ($list -and $list.items -and $list.items.Count -gt 0) { $list.items[0] } else { $null }
  Check '设备列表恰好 1 台' ($list.items.Count -eq 1) "count=$($list.items.Count)"
  Check '设备状态 online（心跳已落库）' ($null -ne $first -and $first.status -eq 'online') $(if ($first) { $first.status } else { '设备缺失' })
  Check '能力摘要已落库' ($null -ne $first -and $null -ne $first.capabilities_hash -and $first.capabilities_hash -ne '') $(if ($first) { $first.capabilities_hash } else { '设备缺失' })
  Check 'last_seen_at 已刷新' ($null -ne $first -and $null -ne $first.last_seen_at) 'last_seen_at 为空'
  $deviceId = if ($first) { $first.id } else { '' }

  # ⑥ 断线 → 复用已配对令牌重连（不重新配对、不重复建设备）
  taskkill /PID $runnerProc.Id /T /F 2>&1 | Out-Null
  Start-Sleep -Milliseconds 800
  Remove-Item -Force $runnerLog -ErrorAction SilentlyContinue
  $runnerProc = StartRunner ("-server '" + $base + "' -token '" + $savedToken + "' -task-delay 100ms -max-retries 3") $runnerLog
  $reconnectText = WaitLog $runnerLog 'FAKE_RUNNER_READY' 25000
  Check '复用设备令牌可重连（无需再配对）' ($reconnectText -match 'FAKE_RUNNER_READY') $reconnectText
  Check '重连未再触发配对' (-not ($reconnectText -match 'FAKE_RUNNER_PAIRED')) $reconnectText

  $list2 = (CallApi 'Get' '/api/v1/devices' $null $token).body
  $first2 = if ($list2 -and $list2.items -and $list2.items.Count -gt 0) { $list2.items[0] } else { $null }
  Check '重连后设备数仍为 1（未重复建设备）' ($list2.items.Count -eq 1) "count=$($list2.items.Count)"
  Check '重连后 device_id 不变' ($null -ne $first2 -and $first2.id -eq $deviceId) "$(if ($first2) { $first2.id } else { '设备缺失' }) vs $deviceId"

  # ⑦ 错误令牌：假 Runner 必须终止重连（不得无限重试刷日志）
  $deadTokenProc = StartRunner ("-server '" + $base + "' -token hgd_deadbeef") '.harness/tmp/e2e-c4-deadtoken.log'
  $deadDone = $deadTokenProc.WaitForExit(20000)
  $deadLog = if (Test-Path '.harness/tmp/e2e-c4-deadtoken.log') { Get-Content -Raw -Encoding UTF8 '.harness/tmp/e2e-c4-deadtoken.log' } else { '' }
  Check '无效设备令牌时假 Runner 退出（不无限重连）' ($deadDone) $deadLog
  Check '无效令牌提示令牌失效' ($deadLog -match '401|失效') $deadLog
  if (-not $deadTokenProc.HasExited) { taskkill /PID $deadTokenProc.Id /T /F 2>&1 | Out-Null }
  Remove-Item -Force '.harness/tmp/e2e-c4-deadtoken.log' -ErrorAction SilentlyContinue

  # ⑧ 吊销设备 → 假 Runner 收 device.revoked 并终止重连
  Remove-Item -Force $runnerLog -ErrorAction SilentlyContinue
  $runnerProc = StartRunner ("-server '" + $base + "' -token '" + $savedToken + "' -task-delay 100ms -max-retries 1") $runnerLog
  $alive = WaitLog $runnerLog 'FAKE_RUNNER_READY' 20000
  Check '吊销前假 Runner 已连上' ($alive -match 'FAKE_RUNNER_READY') $alive
  $revoke = CallApi 'Delete' "/api/v1/devices/$deviceId" $null $token
  Check '吊销设备 204' ($revoke.status -eq 204) $revoke.status
  $revokedLog = WaitLog $runnerLog 'device.revoked' 12000
  Check '假 Runner 收到 device.revoked' ($revokedLog -match 'device.revoked') $revokedLog

  # ⑨ 设备域日志不得泄露设备令牌明文
  Start-Sleep -Milliseconds 400
  $logText = if (Test-Path $logFile) { Get-Content -Raw -Encoding UTF8 $logFile } else { '' }
  Check '服务日志不含 device_token 明文' (-not ($logText -match 'hgd_')) '日志泄露设备令牌'
  Check '服务日志不含用户口令' (-not ($logText -match 'e2e-c4-pass-123')) '日志泄露口令'
}
finally {
  if ($runnerProc -and -not $runnerProc.HasExited) { taskkill /PID $runnerProc.Id /T /F 2>&1 | Out-Null }
  if ($proc -and -not $proc.HasExited) { taskkill /PID $proc.Id /T /F 2>&1 | Out-Null }
}

if ($script:fail -eq 0) { Write-Host 'E2E_C4_RESULT PASS' }
else { Write-Host "E2E_C4_RESULT FAIL ($script:fail 项失败)" }
if ($script:fail -ne 0) { exit 1 }
