@echo off
rem ===========================================================================
rem harness-go one-click launcher for the Windows distribution (zip package).
rem
rem What it does:
rem   1) switches the console to UTF-8 (code page 65001) so logs render right
rem   2) runs harness-go.exe with a data directory next to this script
rem   3) opens the browser once GET /healthz answers
rem
rem Optional arguments are forwarded to the binary, for example:
rem   start.cmd -addr 0.0.0.0:5611            expose to the local network
rem   start.cmd -log-level debug              verbose logging
rem   start.cmd -config my.yaml               use a custom config file
rem
rem Stop: Ctrl+C in this window (the server shuts down gracefully, 30s budget).
rem
rem This file is intentionally ASCII-only: cmd.exe reads .cmd files using the OEM
rem code page, so non-ASCII text here would be garbled in a default console.

setlocal
cd /d "%~dp0"

rem Switch the console to UTF-8. System32 is prepended to PATH first: when this
rem script is started detached (WMI / Task Scheduler) PATH may lack System32 and
rem a bare "chcp" would print a garbled "'chcp' is not recognized" line.
set "PATH=%SystemRoot%\System32;%PATH%"
set "OLDCP="
for /f "tokens=2 delims=:" %%A in ('chcp') do set "OLDCP=%%A"
if defined OLDCP chcp 65001 >nul 2>nul

if not exist "harness-go.exe" (
  echo [ERROR] harness-go.exe not found next to this script.
  echo         Please extract the whole zip archive and run start.cmd from there.
  call :pause_on_failure
  goto :restore
)

rem Use a per-package data directory unless the caller passes -data explicitly.
if not exist "data" mkdir "data"

rem Derive the port from a forwarded "-addr host:port" argument so the browser
rem probe opens the right URL (default 127.0.0.1:5611). Only the last colon
rem component is used, which also handles "-addr 0.0.0.0:9000" and "[::]:9000".
set "ADDR=127.0.0.1:5611"
set "PREV="
rem Guard required: "for %%A in () do" is a cmd syntax error when no args given.
if not "%~1"=="" for %%A in (%*) do call :scan_arg "%%~A"
set "PORT=%ADDR%"
for %%P in (%ADDR::= %) do set "PORT=%%P"
if "%PORT%"=="" set "PORT=5611"
set "BASE_URL=http://127.0.0.1:%PORT%"

rem Probe /healthz in a detached helper and open the browser once it is ready.
rem 60 x 500ms = 30s upper bound; silently gives up after that.
start "" /b powershell -NoProfile -ExecutionPolicy Bypass -Command ^
  "for ($i = 0; $i -lt 60; $i++) { try { $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 '%BASE_URL%/healthz'; if ($r.StatusCode -eq 200) { Start-Process '%BASE_URL%/'; break } } catch { Start-Sleep -Milliseconds 500 } }"

echo Starting harness-go ... press Ctrl+C to stop.
echo URL: %BASE_URL%   (data directory: %CD%\data)
if exist "data\initial_password" (
  echo Initial account password: see data\initial_password
)
echo.

harness-go.exe -data "data" %*

set "RC=%ERRORLEVEL%"
echo.
echo harness-go exited with code %RC%.

:restore
if defined OLDCP "%CHCP%" %OLDCP% >nul 2>nul
exit /b 0

:scan_arg
rem Detect "-addr VALUE" / "--addr VALUE" / "-addr=VALUE"; ignore everything else.
if /i "%PREV%"=="-addr"  set "ADDR=%~1"
if /i "%PREV%"=="--addr" set "ADDR=%~1"
set "PREV="
set "RAW=%~1"
if /i "%RAW:~0,6%"=="-addr="  set "ADDR=%RAW:~6%"
if /i "%RAW:~0,7%"=="--addr=" set "ADDR=%RAW:~7%"
if /i "%RAW%"=="-addr"  set "PREV=-addr"
if /i "%RAW%"=="--addr" set "PREV=--addr"
exit /b 0

:pause_on_failure
echo.
echo Press Enter to close this window...
pause >nul
exit /b 0
