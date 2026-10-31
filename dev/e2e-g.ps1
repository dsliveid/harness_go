# 阶段 G 端到端验收：快照回滚（S10）+ 长任务检查点 + SOP 全量注入 + 会话导出
#
# 用法：powershell -ExecutionPolicy Bypass -File dev/e2e-g.ps1
# 输出末尾固定为 E2E_G_RESULT PASS|FAIL。
#
# 只验证「真实二进制」的联调面（状态机/回滚细节由单测与 cmd 集成测试覆盖）：
#   1) 启动日志含「阶段 G 能力已接线」；/api/v1/tools 含 37 个工具（F 30 + G 7）
#   2) 对话发起 write_file（fake provider 回工具帧？——fake provider 不发工具帧，
#      故写入面用 save_sop 的文件真源 + 快照 API 验证；见脚本内注）
#   3) SOP：save_sop 工具不可直调（工具调用走模型），改用 API 层验证
#      snapshots 空列表与 rollbacks 空列表可达（装配不 500）

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$port = if ($env:E2E_G_PORT) { $env:E2E_G_PORT } else { '18097' }
$dataDir = '.harness/tmp/e2e-g-data'
$logFile = '.harness/tmp/e2e-g.out.log'
$exe = '.harness/tmp/harness-go-g.exe'

New-Item -ItemType Directory -Force -Path '.harness/tmp' | Out-Null
Remove-Item -Recurse -Force $dataDir -ErrorAction SilentlyContinue
Remove-Item -Force $logFile -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

Write-Host '== 构建二进制 =='
go build -o $exe ./cmd/harness-go
if ($LASTEXITCODE -ne 0) { Write-Host 'E2E_G_RESULT FAIL'; exit 1 }

$env:HARNESS_USER = 'admin'
$env:HARNESS_PWD = 'e2e-g-pass-123'

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
  $params = @{ Uri = "$base$path"; Method = $method; Headers = $headers; UseBasicParsing = $true }
  if ($null -ne $body) {
    $params['ContentType'] = 'application/json'
    $params['Body'] = ($body | ConvertTo-Json -Depth 8 -Compress)
  }
  try {
    $r = Invoke-WebRequest @params
    return @{ status = [int]$r.StatusCode; body = ($r.Content | ConvertFrom-Json) }
  } catch {
    $code = 0
    if ($_.Exception.Response) { $code = [int]$_.Exception.Response.StatusCode }
    return @{ status = $code; body = $null }
  }
}

try {
  # ① 启动日志：G 能力接线 + 插件装配
  $log = Get-Content $logFile -Raw -Encoding UTF8 -ErrorAction SilentlyContinue
  Check '启动日志含 G 接线' ($log -match 'snapshots=global-cas') ($log | Select-Object -First 1)
  Check '插件装配无失败' (-not ($log -match '内置插件装配失败')) '有插件装配失败'

  # ② 登录
  $login = CallApi 'Post' '/api/v1/auth/login' @{ username = 'admin'; password = 'e2e-g-pass-123' } $null
  Check '登录成功' ($login.status -eq 200 -and $login.body.token) "status=$($login.status)"
  $token = $login.body.token

  # ③ 工具清单：37 个（F 30 + long-task 3 + sop 2 + session-bundle 2）
  #    响应形状 registry.Snapshot：total + core/deferred/on_demand 分组
  $tools = CallApi 'Get' '/api/v1/tools' $null $token
  $count = $tools.body.total
  Check '工具总数 37' ($count -eq 37) "total=$count"
  $allSpecs = @()
  foreach ($grp in @('core','deferred','on_demand')) { $allSpecs += @($tools.body.$grp) }
  $names = @($allSpecs | ForEach-Object { $_.name })
  Check '含 start_long_task' ($names -contains 'start_long_task') ''
  Check '含 save_sop' ($names -contains 'save_sop') ''
  Check '含 export_session' ($names -contains 'export_session') ''
  Check '含 write_file' ($names -contains 'write_file') ''

  # ④ 会话创建（201）+ G API 可达（空会话快照/回滚列表为空，不 500）
  $sess = CallApi 'Post' '/api/v1/sessions' @{ title = 'G 冒烟' } $token
  Check '创建会话' ($sess.status -eq 201) "status=$($sess.status)"
  $sid = $sess.body.id

  $snaps = CallApi 'Get' "/api/v1/sessions/$sid/snapshots" $null $token
  Check '快照列表可达' ($snaps.status -eq 200 -and $snaps.body.items.Count -eq 0) "status=$($snaps.status)"
  $rbs = CallApi 'Get' "/api/v1/sessions/$sid/rollbacks" $null $token
  Check '回滚列表可达' ($rbs.status -eq 200 -and $rbs.body.items.Count -eq 0) "status=$($rbs.status)"

  # ⑤ 非法回滚请求显式 400（mode 校验，不静默）
  $bad = CallApi 'Post' "/api/v1/sessions/$sid/rollback" @{ mode = 'bogus' } $token
  Check '非法 mode 显式 400' ($bad.status -eq 400) "status=$($bad.status)"

  # ⑥ 健康检查
  $health = CallApi 'Get' '/healthz' $null $null
  Check '健康检查' ($health.status -eq 200) "status=$($health.status)"

  Write-Host "== 冒烟完成：fail=$script:fail =="
} finally {
  # 清理不抛错（进程可能已随 Stop-Process 退出，taskkill /T 找不到进程是正常路径）
  try { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue } catch {}
  try { cmd /c "taskkill /T /F /PID $($proc.Id)" 2>$null | Out-Null } catch {}
}

if ($script:fail -gt 0) { Write-Host 'E2E_G_RESULT FAIL'; exit 1 }
Write-Host 'E2E_G_RESULT PASS'
