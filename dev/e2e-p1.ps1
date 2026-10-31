# P1 端到端验收：OpenAI 兼容端点 + Webhook 事件订阅
#
# 用法：powershell -ExecutionPolicy Bypass -File dev/e2e-p1.ps1
# 输出末尾固定为 E2E_P1_RESULT PASS|FAIL。
#
# 验证面（真实二进制）：
#   1) GET /v1/models（Bearer API Token）返回 list + default
#   2) POST /v1/chat/completions 非流式：chat.completion 形状 + usage
#   3) POST /v1/chat/completions 流式：SSE chunk 帧 + [DONE]
#   4) 无令牌 401
#   5) Webhook CRUD：创建（secret 脱敏）→ enable/disable → test（webhook.test）→ 删除
#   6) 会话列表不含 openai 隐藏会话（对话后验证）

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$port = if ($env:E2E_P1_PORT) { $env:E2E_P1_PORT } else { '18098' }
$dataDir = '.harness/tmp/e2e-p1-data'
$logFile = '.harness/tmp/e2e-p1.out.log'
$exe = '.harness/tmp/harness-go-p1.exe'

New-Item -ItemType Directory -Force -Path '.harness/tmp' | Out-Null
Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue
Remove-Item -Force $logFile -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

Write-Host '== 构建二进制 =='
go build -o $exe ./cmd/harness-go
if ($LASTEXITCODE -ne 0) { Write-Host 'E2E_P1_RESULT FAIL'; exit 1 }

$env:HARNESS_USER = 'admin'
$env:HARNESS_PWD = 'e2e-p1-pass-123'

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

function CallApi($method, $path, $body, $token, $headers) {
  $h = @{}
  if ($token) { $h['Authorization'] = "Bearer $token" }
  if ($headers) { foreach ($k in $headers.Keys) { $h[$k] = $headers[$k] } }
  $params = @{ Uri = "$base$path"; Method = $method; Headers = $h; UseBasicParsing = $true }
  if ($null -ne $body) {
    $params['ContentType'] = 'application/json'
    $params['Body'] = ($body | ConvertTo-Json -Depth 8 -Compress)
  }
  try {
    $r = Invoke-WebRequest @params
    return @{ status = [int]$r.StatusCode; body = $r.Content; headers = $r.Headers }
  } catch {
    $code = 0; $respBody = ''
    if ($_.Exception.Response) {
      $code = [int]$_.Exception.Response.StatusCode
      try { $respBody = (New-Object IO.StreamReader($_.Exception.Response.GetResponseStream())).ReadToEnd() } catch {}
    }
    return @{ status = $code; body = $respBody; headers = $null }
  }
}

try {
  # ① 登录
  $login = CallApi 'Post' '/api/v1/auth/login' @{ username = 'admin'; password = 'e2e-p1-pass-123' } $null
  Check '登录成功' ($login.status -eq 200 -and $login.body) "status=$($login.status)"
  $loginJson = $login.body | ConvertFrom-Json
  $token = $loginJson.token

  # ② GET /v1/models
  $models = CallApi 'Get' '/v1/models' $null $token
  Check 'GET /v1/models 200' ($models.status -eq 200) "status=$($models.status)"
  $modelsJson = $models.body | ConvertFrom-Json
  Check 'models 形状 list' ($modelsJson.object -eq 'list' -and $modelsJson.data.Count -ge 1) ($models.body | Out-String)
  $hasDefault = $false
  foreach ($m in $modelsJson.data) { if ($m.id -eq 'default') { $hasDefault = $true } }
  Check '含 default 模型' $hasDefault ''

  # ③ 非流式补全（fake provider 回显模式）
  $chat = CallApi 'Post' '/v1/chat/completions' @{ model = 'default'; messages = @(@{ role = 'user'; content = '你好' }) } $token
  Check 'POST chat/completions 200' ($chat.status -eq 200) "status=$($chat.status)"
  $chatJson = $chat.body | ConvertFrom-Json
  Check 'chat.completion 形状' ($chatJson.object -eq 'chat.completion' -and $chatJson.choices.Count -eq 1) ''
  Check 'usage 已回写' ($chatJson.usage.total_tokens -ge 0) ''
  Check 'assistant 回复非空' ($chatJson.choices[0].message.content.Length -gt 0) ''

  # ④ 流式补全（SSE）
  $stream = CallApi 'Post' '/v1/chat/completions' @{ model = 'default'; stream = $true; messages = @(@{ role = 'user'; content = '讲个故事' }) } $token
  $streamBody = $stream.body -join "`n"
  Check '流式 200 + chunk 帧' ($stream.status -eq 200 -and $streamBody -match 'chat\.completion\.chunk') ''
  Check '流式含 [DONE]' ($streamBody -match '\[DONE\]') ''

  # ⑤ 无令牌 401
  $noauth = CallApi 'Get' '/v1/models' $null $null
  Check '无令牌 401' ($noauth.status -eq 401) "status=$($noauth.status)"

  # ⑥ 会话列表不含 openai 隐藏会话
  $sessions = CallApi 'Get' '/api/v1/sessions' $null $token
  $sessJson = $sessions.body | ConvertFrom-Json
  $leaked = $false
  foreach ($s in $sessJson.items) { if ($s.source -eq 'openai') { $leaked = $true } }
  Check '隐藏会话不进默认列表' (-not $leaked) ''

  # ⑦ Webhook CRUD + test
  $wh = CallApi 'Post' '/api/v1/webhooks' @{ url = 'http://127.0.0.1:9/nowhere'; secret = 'sig-secret'; events = @('run.completed') } $token
  Check '创建 Webhook 201' ($wh.status -eq 201) "status=$($wh.status)"
  $whJson = $wh.body | ConvertFrom-Json
  Check 'secret 脱敏' (-not ($wh.body -match 'sig-secret') -and $whJson.has_secret) ''
  $test = CallApi 'Post' "/api/v1/webhooks/$($whJson.id)/test" $null $token
  Check 'test 端点可达（连接失败也回结构化结果）' ($test.status -eq 200 -and ($test.body -match '"ok":false')) ($test.body)
  $dis = CallApi 'Post' "/api/v1/webhooks/$($whJson.id)/disable" $null $token
  Check 'disable 204' ($dis.status -eq 204) "status=$($dis.status)"
  $en = CallApi 'Post' "/api/v1/webhooks/$($whJson.id)/enable" $null $token
  Check 'enable 204' ($en.status -eq 204) "status=$($en.status)"
  $list = CallApi 'Get' '/api/v1/webhooks' $null $token
  Check '列表含订阅' ($list.body -match $whJson.id) ''
  $del = CallApi 'Delete' "/api/v1/webhooks/$($whJson.id)" $null $token
  Check '删除 204' ($del.status -eq 204) "status=$($del.status)"

  Write-Host "== 冒烟完成：fail=$script:fail =="
} finally {
  try { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue } catch {}
  try { cmd /c "taskkill /T /F /PID $($proc.Id)" 2>$null | Out-Null } catch {}
}

if ($script:fail -gt 0) { Write-Host 'E2E_P1_RESULT FAIL'; exit 1 }
Write-Host 'E2E_P1_RESULT PASS'
