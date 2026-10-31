# 阶段 C-3 端到端验收：WS 网关（鉴权在升级前 / welcome / ping-pong / 45s 离线 / 吊销断连 / token 日志脱敏）
#
# 用法：powershell -ExecutionPolicy Bypass -File dev/e2e-c3.ps1
# 输出末尾固定为 E2E_C3_RESULT PASS|FAIL，便于上层断言。
#
# 说明：
#   1) 不用 curl.exe —— PowerShell 给原生程序传参时剥离 JSON 双引号。统一 Invoke-WebRequest。
#   2) WS 客户端用 .NET ClientWebSocket（PS 5.1 自带，零额外依赖）。
#   3) 服务输出经 powershell 子进程 `*>` 重定向到文件（避免 run_command 因句柄继承挂起）。
#   4) 收尾用 taskkill /T 杀进程树。

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$port = if ($env:E2E_C3_PORT) { $env:E2E_C3_PORT } else { '18093' }
$dataDir = '.harness/tmp/e2e-c3-data'
$logFile = '.harness/tmp/e2e-c3.out.log'
$exe = '.harness/tmp/harness-go-c3.exe'

New-Item -ItemType Directory -Force -Path '.harness/tmp' | Out-Null
Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue
Remove-Item -Force $logFile -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

Write-Host '== 构建二进制 =='
go build -o $exe ./cmd/harness-go
if ($LASTEXITCODE -ne 0) { Write-Host 'E2E_C3_RESULT FAIL'; exit 1 }

$env:HARNESS_USER = 'admin'
$env:HARNESS_PWD = 'e2e-c3-pass-123'

Write-Host "== 启动服务（端口 $port）=="
$launcher = "& '" + $exe + "' -data '" + $dataDir + "' -addr '127.0.0.1:" + $port + "' *> '" + $logFile + "'"
$proc = Start-Process -FilePath 'powershell' -ArgumentList '-NoProfile', '-NonInteractive', '-Command', $launcher -PassThru -WindowStyle Hidden
Start-Sleep -Seconds 3

$base = "http://127.0.0.1:$port"
$script:fail = 0

function Check($name, $cond, $detail) {
  if ($cond) { Write-Host "  PASS  $name" }
  else { Write-Host "  FAIL  $name  -> $detail"; $script:fail++ }
}

function CallApi($method, $path, $body, $token) {
  $headers = @{}
  if ($token) { $headers['Authorization'] = "Bearer $token" }
  $params = @{
    Uri             = "$base$path"
    Method          = $method
    Headers         = $headers
    UseBasicParsing = $true
  }
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
    $code = [int]$_.Exception.Response.StatusCode
    $parsed = $null
    if ($content) { $parsed = $content | ConvertFrom-Json }
    return @{ status = $code; raw = $content; body = $parsed }
  }
}

# --- 极简 WS 客户端封装（同步等待，超时可判定） ---
function WsConnect($url, $timeoutMs) {
  $ws = New-Object System.Net.WebSockets.ClientWebSocket
  $ct = New-Object System.Threading.CancellationTokenSource
  $ct.CancelAfter($timeoutMs)
  try {
    $ws.ConnectAsync([Uri]$url, $ct.Token).Wait($timeoutMs) | Out-Null
    return @{ ok = $true; ws = $ws; error = '' }
  }
  catch {
    # AggregateException → WebSocketException 多层包裹，递归取最内层消息（含状态码）
    $e = $_.Exception
    $msg = $e.Message
    while ($e.InnerException) { $e = $e.InnerException; $msg = $e.Message }
    return @{ ok = $false; ws = $ws; error = $msg }
  }
}

function WsSendText($ws, $text, $timeoutMs) {
  $bytes = [System.Text.Encoding]::UTF8.GetBytes($text)
  $seg = New-Object System.ArraySegment[byte] -ArgumentList @(,$bytes)
  $ct = New-Object System.Threading.CancellationTokenSource
  $ct.CancelAfter($timeoutMs)
  $ws.SendAsync($seg, [System.Net.WebSockets.WebSocketMessageType]::Text, $true, $ct.Token).Wait($timeoutMs) | Out-Null
}

function WsRecvText($ws, $timeoutMs) {
  $buf = New-Object byte[] 8192
  $seg = New-Object System.ArraySegment[byte] -ArgumentList @(,$buf)
  $ct = New-Object System.Threading.CancellationTokenSource
  $ct.CancelAfter($timeoutMs)
  $ms = New-Object System.IO.MemoryStream
  try {
    do {
      $t = $ws.ReceiveAsync($seg, $ct.Token)
      $t.Wait($timeoutMs) | Out-Null
      $res = $t.Result
      if ($res.MessageType -eq [System.Net.WebSockets.WebSocketMessageType]::Close) { return $null }
      $ms.Write($buf, 0, $res.Count)
    } while (-not $res.EndOfMessage)
    return [System.Text.Encoding]::UTF8.GetString($ms.ToArray())
  }
  catch {
    return $null
  }
}

try {
  # ① 登录 → 配对 → 拿 device_token
  $login = CallApi 'Post' '/api/v1/auth/login' @{ username = 'admin'; password = 'e2e-c3-pass-123' } $null
  $token = $login.body.token
  Check '登录获得用户令牌' ($login.status -eq 200 -and $token) $login.raw

  $pc = (CallApi 'Post' '/api/v1/devices/pair-codes' @{ name = 'C3 桌面'; kind = 'desktop' } $token).body
  $paired = (CallApi 'Post' '/api/v1/devices/pair' @{
      code = $pc.code; name = 'C3 端'; kind = 'desktop'; platform = 'windows'
      capabilities = @(@{ name = 'fs.read'; version = '1.0' })
    } $null).body
  $dtoken = $paired.device_token
  $did = $paired.device_id
  Check '配对获得 device_token' ($dtoken -like 'hgd_*') $dtoken

  $wsUrl = "ws://127.0.0.1:$port/api/v1/devices/ws"

  # ② 鉴权在升级前：无令牌 / 错误令牌必须 401（不是 101）
  $r1 = WsConnect $wsUrl 4000
  Check '无令牌升级被拒且为 401（非 101）' ((-not $r1.ok) -and ($r1.error -match '401')) $r1.error
  $r2 = WsConnect ($wsUrl + '?token=hgd_not_exists') 4000
  Check '错误令牌升级被拒且为 401' ((-not $r2.ok) -and ($r2.error -match '401')) $r2.error

  # ③ 正确令牌 → 升级成功，首帧 welcome
  $r3 = WsConnect ($wsUrl + '?token=' + $dtoken) 6000
  Check '正确令牌升级成功' ($r3.ok) $r3.error
  if (-not $r3.ok) { throw 'WS 升级失败，后续断言无法进行' }
  $ws = $r3.ws

  $welcomeRaw = WsRecvText $ws 5000
  Check '首帧收到 welcome' ($null -ne $welcomeRaw -and $welcomeRaw -match '"type":"welcome"') $welcomeRaw
  $welcome = $welcomeRaw | ConvertFrom-Json
  Check 'welcome 协议版本 = 1' ($welcome.payload.protocol_version -eq 1) $welcome.payload.protocol_version
  Check 'welcome 心跳间隔 = 15s' ($welcome.payload.heartbeat_s -eq 15) $welcome.payload.heartbeat_s
  Check 'welcome 离线阈值 = 45s' ($welcome.payload.offline_after_s -eq 45) $welcome.payload.offline_after_s
  Check 'welcome 回显 device_id' ($welcome.payload.device_id -eq $did) $welcome.payload.device_id

  # ④ ping → pong，且设备列表转为 online 并写回能力摘要
  WsSendText $ws '{"v":1,"type":"ping","id":"p-1","payload":{"load":{"cpu":9},"capabilities_hash":"e2e-hash","protocol_version":1}}' 5000
  $pongRaw = WsRecvText $ws 5000
  Check 'ping 得到 pong 应答' ($null -ne $pongRaw -and $pongRaw -match '"type":"pong"' -and $pongRaw -match '"id":"p-1"') $pongRaw

  Start-Sleep -Milliseconds 300
  $list = (CallApi 'Get' '/api/v1/devices' $null $token).body
  Check 'ping 后设备状态 online' ($list.items[0].status -eq 'online') $list.items[0].status
  Check 'ping 的 capabilities_hash 已落库' ($list.items[0].capabilities_hash -eq 'e2e-hash') $list.items[0].capabilities_hash
  Check 'last_seen_at 已刷新' ($null -ne $list.items[0].last_seen_at) 'last_seen_at 为空'

  # ⑤ 非法帧 → error{device.bad_frame}，连接保持
  WsSendText $ws '{"v":1,"type":"unknown_frame"}' 5000
  $badRaw = WsRecvText $ws 5000
  Check '非法帧回 device.bad_frame' ($null -ne $badRaw -and $badRaw -match 'device.bad_frame') $badRaw

  # ⑥ 协议版本不兼容 → error{device.protocol_mismatch, fatal} + 关闭
  $r4 = WsConnect ($wsUrl + '?token=' + $dtoken + '&protocol_version=99') 6000
  Check '不兼容版本仍完成握手（101 后立即关）' ($r4.ok) $r4.error
  if ($r4.ok) {
    $mmRaw = WsRecvText $r4.ws 5000
    Check '不兼容版本回 protocol_mismatch' ($null -ne $mmRaw -and $mmRaw -match 'device.protocol_mismatch') $mmRaw
    Check '不兼容版本后连接被关闭' ($null -eq (WsRecvText $r4.ws 3000)) '连接未关闭'
  }

  # ⑦ 同设备重复连接 → 旧连接收 device.replaced 并被断开
  $r5 = WsConnect ($wsUrl + '?token=' + $dtoken) 6000
  Check '同设备二次连接成功（新连接接管）' ($r5.ok) $r5.error
  if ($r5.ok) {
    $replacedRaw = WsRecvText $ws 5000
    Check '旧连接收到 device.replaced' ($null -ne $replacedRaw -and $replacedRaw -match 'device.replaced') $replacedRaw
    $welcome2 = WsRecvText $r5.ws 5000
    Check '新连接收到 welcome' ($null -ne $welcome2 -and $welcome2 -match '"type":"welcome"') $welcome2
  }

  # ⑧ 吊销 → 长连接被主动断开（服务端先发 device.revoked 错误帧，再关闭）
  $revoke = CallApi 'Delete' "/api/v1/devices/$did" $null $token
  Check '吊销设备 204' ($revoke.status -eq 204) $revoke.status
  if ($r5.ok) {
    $revokedRaw = WsRecvText $r5.ws 4000
    Check '吊销后收到 device.revoked（或连接已断）' (
      ($revokedRaw -match 'device.revoked') -or ($null -eq $revokedRaw)
    ) $revokedRaw
    Check '吊销后连接最终关闭' ($null -eq (WsRecvText $r5.ws 3000)) '连接未断开'
  }

  # ⑨ 日志脱敏铁律：device_token / 用户口令 均不得出现在服务输出
  Start-Sleep -Milliseconds 300
  $logText = if (Test-Path $logFile) { Get-Content -Raw -Encoding UTF8 $logFile } else { '' }
  Check '日志含 access 记录（服务确有输出）' ($logText -match 'access') '日志为空或无 access 行'
  Check '日志不含 device_token 明文' (-not ($logText -match 'hgd_')) '日志泄露设备令牌'
  Check '日志不含用户口令' (-not ($logText -match 'e2e-c3-pass-123')) '日志泄露口令'
  # access 行必须完全不含 token 键（bootstrap 一次性打印初始用户 API 令牌属既定设计，不在 access 行内）
  $accessLines = ($logText -split "`n") | Where-Object { $_ -match 'msg=access' }
  Check '存在 access 行可断言' ($accessLines.Count -gt 0) '未找到 access 行'
  $leaky = $accessLines | Where-Object { $_ -match 'token' }
  Check 'access 行不含 token 键（query 脱敏生效）' ($null -eq $leaky) ($leaky -join ' | ')
}
finally {
  if ($proc -and -not $proc.HasExited) {
    taskkill /PID $proc.Id /T /F 2>&1 | Out-Null
  }
}

if ($script:fail -eq 0) { Write-Host 'E2E_C3_RESULT PASS' }
else { Write-Host "E2E_C3_RESULT FAIL ($script:fail 项失败)" }
