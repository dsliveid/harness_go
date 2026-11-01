@echo off
rem harness-go launcher for Windows.
rem
rem Usage: run.cmd [options]   (options are forwarded to run.sh, e.g. --dev / --port 9000)
rem
rem The project scripts are written in bash, which is not on the Windows PATH by
rem default, so this launcher locates Git Bash automatically:
rem   1) bash.exe already on PATH
rem   2) derived from git.exe on PATH   <Git>\cmd\git.exe -> <Git>\bin\bash.exe
rem   3) common install locations
rem It also switches the console to UTF-8 (code page 65001) so the Chinese output
rem of run.sh renders correctly, and restores the previous code page on exit.
rem
rem This file is intentionally ASCII-only: cmd.exe reads .cmd files using the OEM
rem code page, so non-ASCII text here would be garbled in a default console.
rem
rem Note: no "pause on failure" here. The server runs in the foreground, so the
rem window must stay open for the whole process lifetime; pressing Ctrl+C ends it
rem normally and the window closes right away.

setlocal
set "HERE=%~dp0"
set "BASH_EXE="

rem 1) bash.exe on PATH
for %%I in (bash.exe) do if not defined BASH_EXE set "BASH_EXE=%%~$PATH:I"
rem 2) derive from git.exe on PATH
for %%I in (git.exe) do if not defined BASH_EXE call :from_git "%%~$PATH:I"
rem 3) common install locations
if not defined BASH_EXE call :try "%ProgramFiles%\Git\bin\bash.exe"
if not defined BASH_EXE call :try "%LOCALAPPDATA%\Programs\Git\bin\bash.exe"
if not defined BASH_EXE call :try "%ProgramFiles(x86)%\Git\bin\bash.exe"

if not defined BASH_EXE (
  echo [ERROR] Git Bash ^(bash.exe^) not found.
  echo.
  echo Install Git for Windows: https://git-scm.com/download/win
  echo or add Git's bin directory to PATH, then retry.
  call :pause_on_failure
  exit /b 1
)

rem remember current code page, switch to UTF-8 for run.sh output.
rem System32 is prepended to PATH first: when this script is started detached
rem (WMI / Task Scheduler / service wrapper) PATH may lack System32, and a bare
rem "chcp" then fails with a garbled "'chcp' is not recognized" line.
rem (Do NOT wrap it as ('"%CHCP%" 2^>nul'): cmd treats the quoted path plus the
rem  redirect as one command name and fails the same way.)
set "PATH=%SystemRoot%\System32;%PATH%"
set "OLDCP="
for /f "tokens=2 delims=:" %%A in ('chcp') do set "OLDCP=%%A"
if defined OLDCP chcp 65001 >nul 2>nul

"%BASH_EXE%" "%HERE%run.sh" %*
set "RC=%ERRORLEVEL%"

if defined OLDCP chcp %OLDCP% >nul 2>nul
exit /b %RC%

:pause_on_failure
rem Keep the window open on failure. The message is ASCII on purpose: cmd.exe reads
rem .cmd files using the OEM code page, so Chinese text here would be garbled.
echo.
echo Press Enter to close this window...
pause >nul
exit /b 0

:try
rem resolve to a full path first: cmd's "if exist" does not collapse ".." segments
for %%I in ("%~1") do if not defined BASH_EXE if exist "%%~fI" set "BASH_EXE=%%~fI"
exit /b 0

:from_git
rem %~dp1 is git.exe's directory and ends with a backslash.
rem Layout A: <Git>\cmd\git.exe  -> one level up
rem Layout B: <Git>\bin\git.exe  -> same directory
if not defined BASH_EXE call :try "%~dp1..\bin\bash.exe"
if not defined BASH_EXE call :try "%~dp1bash.exe"
exit /b 0
