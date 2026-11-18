@echo off
rem Switch the console to UTF-8, otherwise the Chinese in this file will be garbled on a GBK terminal.
chcp 65001 >nul 2>&1
rem ARTEX daemon startup script (Windows)
rem
rem Usage:
rem   start.bat                  run in foreground (Ctrl-C to stop)
rem   start.bat -addr :9000      extra arguments are passed through to artex as-is
rem
rem It does only one thing: run artex.exe, and after the process exits decide by exit code whether to relaunch.
rem
rem   0      user stopped normally     -> exit the loop
rem   75     Program requests restart     -> rerun immediately (page clicked "one-click update" or "rollback")
rem   other  crash                     -> rerun after backoff (1->2->4… up to 60 seconds)
rem
rem Download, SHA256 verification, and swapping are not here; all done by artex itself at startup
rem (selfupdate package). The script stays foolproof; see the notes at the top of start.sh for details.

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] cannot find executable %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] exited normally
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem update/rollback is ready: after rerun artex will complete the swap at startup.
	echo [artex] requesting restart (applying new version)…
	set /a delay=1
	goto loop
)

echo [artex] abnormal exit ^(code=!code!^), restarting after !delay!s 1>&2
rem timeout fails in a redirected console, use ping as a fallback (a delay of N seconds needs N+1 pings).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
