@echo off
rem ---------------------------------------------------------------------------
rem harness-go launcher for Windows (thin wrapper).
rem
rem Usage: run.cmd [options]   (options are forwarded to run.sh,
rem                            e.g. --dev / --port 9000 / --open)
rem
rem All the Git Bash discovery + UTF-8 code page logic lives in the shared
rem scripts\win\bash.cmd launcher. This file only forwards the target script
rem and the raw argument string.
rem
rem Note: no "pause on failure" here. The server runs in the foreground, so the
rem window must stay open for the whole process lifetime; pressing Ctrl+C ends
rem it normally and the window closes right away.
rem
rem This file is intentionally ASCII-only: cmd.exe reads .cmd files using the OEM
rem code page, so non-ASCII text here would be garbled in a default console.
rem ---------------------------------------------------------------------------

setlocal

set "HARNESS_SCRIPT=%~dp0run.sh"
set "HARNESS_ARGS=%*"

call "%~dp0scripts\win\bash.cmd"
exit /b %ERRORLEVEL%
