@echo off
rem ---------------------------------------------------------------------------
rem harness-go build launcher for Windows (thin wrapper).
rem
rem Usage: build.cmd [options]   (options are forwarded to build.sh,
rem                              e.g. --web / --skip-web / --debug)
rem
rem All the Git Bash discovery + UTF-8 code page logic lives in the shared
rem scripts\win\bash.cmd launcher. This file only forwards the target script
rem and the raw argument string.
rem
rem This file is intentionally ASCII-only: cmd.exe reads .cmd files using the OEM
rem code page, so non-ASCII text here would be garbled in a default console.
rem ---------------------------------------------------------------------------

setlocal

set "HARNESS_SCRIPT=%~dp0build.sh"
set "HARNESS_ARGS=%*"
set "HARNESS_PAUSE_ON_FAIL=1"

call "%~dp0scripts\win\bash.cmd"
exit /b %ERRORLEVEL%
