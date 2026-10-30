# B-6 regression launcher: build artifact + real binary (for headless browser checks).
# Usage: powershell -ExecutionPolicy Bypass -File dev/run-b6.ps1

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$env:HARNESS_USER = 'admin'
$env:HARNESS_PWD = 'e2e-pass-123'

$exe = Join-Path $root 'bin\harness-go.exe'
$data = Join-Path $root '.harness\tmp\b6-data'
$outLog = Join-Path $root '.harness\tmp\b6.out.log'
$errLog = Join-Path $root '.harness\tmp\b6.err.log'

if (Test-Path $data) { Remove-Item -Recurse -Force $data }
New-Item -ItemType Directory -Force -Path (Split-Path $data -Parent) | Out-Null

Start-Process -FilePath $exe -ArgumentList '-data', $data, '-addr', '127.0.0.1:18090' -RedirectStandardOutput $outLog -RedirectStandardError $errLog -WindowStyle Hidden

$conn = $false
for ($i = 0; $i -lt 50; $i++) {
  $conn = Test-NetConnection -ComputerName '127.0.0.1' -Port 18090 -InformationLevel Quiet -WarningAction SilentlyContinue
  if ($conn) { break }
  Start-Sleep -Milliseconds 300
}

try {
  $probe = Invoke-WebRequest -Uri 'http://127.0.0.1:18090/healthz' -UseBasicParsing -TimeoutSec 5
  Write-Output ("HEALTH_STATUS " + $probe.StatusCode)
  Write-Output ("HEALTH_BODY " + $probe.Content)
} catch {
  Write-Output ("HEALTH_FAIL " + $_.Exception.Message)
  exit 1
}
