@echo off
rem ---------------------------------------------------------------------------
rem harness-go frontend dev server launcher for Windows (thin wrapper).
rem
rem Usage: web\run.cmd [options]   (options are forwarded to web\run.sh,
rem                                 e.g. --port 5612 / --real / --backend http://127.0.0.1:5611)
rem
rem All the Git Bash discovery + UTF-8 code page logic lives in the shared
rem scripts\win\bash.cmd launcher. This file only forwards the target script
rem and the raw argument string.
rem
rem Note: no "pause on failure" here. vite runs in the foreground, so the window
rem must stay open for the whole process lifetime; Ctrl+C ends it normally.
rem
rem This file is intentionally ASCII-only: cmd.exe reads .cmd files using the OEM
rem code page, so non-ASCII text here would be garbled in a default console.
rem ---------------------------------------------------------------------------

setlocal

set "HARNESS_SCRIPT=%~dp0run.sh"
set "HARNESS_ARGS=%*"

call "%~dp0..\scripts\win\bash.cmd"
exit /b %ERRORLEVEL%
