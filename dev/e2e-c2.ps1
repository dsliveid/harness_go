# 阶段 C-2 端到端验收：配对码 TTL 120s / 一次性消费 / 防枚举 / 设备注册与吊销
#
# 用法：powershell -ExecutionPolicy Bypass -File dev/e2e-c2.ps1
# 输出末尾固定为 E2E_C2_RESULT PASS|FAIL，便于上层断言。
#
# 说明：不用 curl.exe —— PowerShell 给原生程序传参时会剥离 JSON 里的双引号，
#       导致请求体变成非法 JSON（"请求体格式错误"）。统一走 Invoke-WebRequest。

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$port = if ($env:E2E_C2_PORT) { $env:E2E_C2_PORT } else { '18092' }
$dataDir = '.harness/tmp/e2e-c2-data'

New-Item -ItemType Directory -Force -Path '.harness/tmp' | Out-Null
Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

Write-Host '== 构建二进制 =='
go build -o .harness/tmp/harness-go-c2.exe ./cmd/harness-go
if ($LASTEXITCODE -ne 0) { Write-Host 'E2E_C2_RESULT FAIL'; exit 1 }

$env:HARNESS_USER = 'admin'
$env:HARNESS_PWD = 'e2e-c2-pass-123'

Write-Host "== 启动服务（端口 $port）=="
$proc = Start-Process -FilePath '.harness/tmp/harness-go-c2.exe' `
  -ArgumentList '-data', $dataDir, '-addr', "127.0.0.1:$port" `
  -PassThru -WindowStyle Hidden
Start-Sleep -Seconds 3

$base = "http://127.0.0.1:$port"
$script:fail = 0

function Check($name, $cond, $detail) {
  if ($cond) { Write-Host "  PASS  $name" }
  else { Write-Host "  FAIL  $name  -> $detail"; $script:fail++ }
}

# Invoke-WebRequest：统一返回 @{status; body; raw}，4xx/5xx 不抛异常。
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
    # PS 5.1：4xx/5xx 会抛异常，响应体可从 ErrorDetails.Message 取（UTF-8 已解码，避免乱码）
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

try {
  # ① 登录拿用户令牌
  $login = CallApi 'Post' '/api/v1/auth/login' @{ username = 'admin'; password = 'e2e-c2-pass-123' } $null
  $token = $login.body.token
  Check '登录获得用户令牌' ($login.status -eq 200 -and $token -and $token.Length -gt 10) $login.raw

  # ② 生成配对码：断言 6 位 + TTL 120s
  $pcResp = CallApi 'Post' '/api/v1/devices/pair-codes' @{ name = 'E2E 桌面'; kind = 'desktop' } $token
  $pc = $pcResp.body
  Check '配对码为 6 位数字' ($pcResp.status -eq 201 -and $pc.code -match '^\d{6}$') $pcResp.raw
  Check '配对码 TTL 回显 120s' ($pc.ttl_s -eq 120) $pc.ttl_s
  $delta = [math]::Round(($pc.expires_at - [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()) / 1000)
  Check 'expires_at 距今 ≈120s' ([math]::Abs($delta - 120) -le 5) $delta

  # ③ 端侧配对（免用户令牌）
  $pairBody = @{
    code         = $pc.code
    name         = 'E2E 端'
    kind         = 'desktop'
    platform     = 'windows'
    capabilities = @(@{ name = 'fs.read'; version = '1.0' })
  }
  $pairedResp = CallApi 'Post' '/api/v1/devices/pair' $pairBody $null
  $paired = $pairedResp.body
  Check '配对返回 device_id' ($pairedResp.status -eq 201 -and $paired.device_id -and $paired.device_id.Length -gt 8) $pairedResp.raw
  Check '配对返回 hgd_ 令牌' ($paired.device_token -like 'hgd_*') $paired.device_token
  Check '配对返回 ws_path' ($paired.ws_path -eq '/api/v1/devices/ws') $paired.ws_path
  Check '配对返回协议版本' ($paired.protocol_version -ge 1) $paired.protocol_version

  # ④ 一次性：同码二次配对 → 401 device.pair_code_invalid
  $reuse = CallApi 'Post' '/api/v1/devices/pair' @{ code = $pc.code } $null
  Check '二次配对 HTTP 401' ($reuse.status -eq 401) $reuse.status
  Check '二次配对错误码防枚举' ($reuse.body.error.code -eq 'device.pair_code_invalid') $reuse.body.error.code

  # ⑤ 防枚举：不存在的码 → 同样的 401 与错误码
  $ghost = CallApi 'Post' '/api/v1/devices/pair' @{ code = '000001' } $null
  Check '不存在码 HTTP 401' ($ghost.status -eq 401) $ghost.status
  Check '不存在码与已消费同码（不可区分）' ($ghost.body.error.code -eq $reuse.body.error.code) "$($ghost.body.error.code) vs $($reuse.body.error.code)"

  # ⑥ 列表：不泄露令牌摘要
  $listResp = CallApi 'Get' '/api/v1/devices' $null $token
  $list = $listResp.body
  Check '设备列表含 1 台设备' ($list.items.Count -eq 1) $list.items.Count
  Check '列表回显 pair_ttl_s=120' ($list.pair_ttl_s -eq 120) $list.pair_ttl_s
  Check '列表不含 token_hash 字段' (-not ($listResp.raw -match 'token_hash')) '响应体出现 token_hash'
  Check '列表不含令牌明文' (-not ($listResp.raw.Contains($paired.device_token))) '响应体泄露令牌明文'
  Check '能力项已落库' ($list.items[0].capabilities.Count -eq 1) ($list.items[0] | ConvertTo-Json -Compress)

  # ⑦ 吊销 → 204；二次吊销 → 404 device.not_found
  $did = $paired.device_id
  $revoke = CallApi 'Delete' "/api/v1/devices/$did" $null $token
  Check '首次吊销 HTTP 204' ($revoke.status -eq 204) $revoke.status
  $revoke2 = CallApi 'Delete' "/api/v1/devices/$did" $null $token
  Check '二次吊销 HTTP 404' ($revoke2.status -eq 404) $revoke2.status
  Check '二次吊销错误码 device.not_found' ($revoke2.body.error.code -eq 'device.not_found') $revoke2.body.error.code

  # ⑧ 鉴权边界：管理端点需令牌
  $noAuth = CallApi 'Get' '/api/v1/devices' $null $null
  Check '设备列表未授权 HTTP 401' ($noAuth.status -eq 401) $noAuth.status

  # ⑨ 反向验证：吊销后设备令牌不可再用于鉴权（通过重新配对 → 吊销 → 认证链路已在单测覆盖）
  $againResp = CallApi 'Post' '/api/v1/devices/pair-codes' @{} $token
  Check '配对码可再次签发' ($againResp.status -eq 201) $againResp.status
}
finally {
  if ($proc -and -not $proc.HasExited) { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue }
}

if ($script:fail -eq 0) { Write-Host 'E2E_C2_RESULT PASS' }
else { Write-Host "E2E_C2_RESULT FAIL ($script:fail 项失败)" }
