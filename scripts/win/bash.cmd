@echo off
rem ---------------------------------------------------------------------------
rem harness-go shared bash launcher for Windows.
rem
rem Do NOT run this directly. It is called by the thin wrappers:
rem   run.cmd / build.cmd / package.cmd / web\run.cmd / web\build.cmd
rem
rem A wrapper only sets these env vars and then calls this file:
rem   HARNESS_SCRIPT         absolute path of the .sh to execute (required)
rem   HARNESS_ARGS           raw argument string forwarded to the .sh (optional)
rem   HARNESS_PAUSE_ON_FAIL  1 = keep the window open when the script fails
rem
rem Why a shared launcher: the Git Bash discovery + UTF-8 code page dance was
rem copy-pasted into three .cmd files (74~79 lines each). Any fix had to be
rem applied three times. It lives here once.
rem
rem This file is intentionally ASCII-only: cmd.exe reads .cmd files using the OEM
rem code page, so non-ASCII text here would be garbled in a default console.
rem ---------------------------------------------------------------------------

setlocal

if not defined HARNESS_SCRIPT (
  echo [ERROR] HARNESS_SCRIPT is not set. Call the wrapper, not this file.
  exit /b 2
)

set "BASH_EXE="

rem 1) bash.exe already on PATH
for %%I in (bash.exe) do if not defined BASH_EXE set "BASH_EXE=%%~$PATH:I"
rem 2) derived from git.exe on PATH   <Git>\cmd\git.exe -> <Git>\bin\bash.exe
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

rem Switch the console to UTF-8 (code page 65001) so the Chinese output of the
rem .sh scripts renders correctly, and restore the previous code page on exit.
rem
rem IMPORTANT: do NOT prepend %SystemRoot%\System32 to PATH here. That was an old
rem workaround for "chcp not found" when started detached, but it hijacks the GNU
rem tools used by the bash scripts: System32\find.exe is a *string search* tool
rem (no -newer) and System32\sort.exe does not support -u. With them on PATH the
rem scripts silently mis-detect stale frontend assets and can delete every asset
rem under web/dist/assets. Calling chcp.com by absolute path fixes the original
rem problem without polluting PATH.
set "OLDCP="
for /f "tokens=2 delims=:" %%A in ('"%SystemRoot%\System32\chcp.com"') do set "OLDCP=%%A"
if defined OLDCP "%SystemRoot%\System32\chcp.com" 65001 >nul 2>nul

if defined HARNESS_ARGS (
  "%BASH_EXE%" "%HARNESS_SCRIPT%" %HARNESS_ARGS%
) else (
  "%BASH_EXE%" "%HARNESS_SCRIPT%"
)
set "RC=%ERRORLEVEL%"

if defined OLDCP "%SystemRoot%\System32\chcp.com" %OLDCP% >nul 2>nul
if not "%RC%"=="0" if defined HARNESS_PAUSE_ON_FAIL call :pause_on_failure
exit /b %RC%

:pause_on_failure
rem Keep the window open on failure. The message is ASCII on purpose: cmd.exe
rem reads .cmd files using the OEM code page, so Chinese text would be garbled.
if not defined HARNESS_PAUSE_ON_FAIL exit /b 0
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
