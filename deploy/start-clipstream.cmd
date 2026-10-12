@echo off
rem ClipStream unattended boot.
rem Deploy ANYWHERE (path-independent): double-click or put a shortcut of this
rem file into the Startup folder. Requires layout:
rem   <root>\bin\clipstream.exe, clipstream-daemon.exe
rem   <root>\config.yaml, <root>\engines\biliup.exe (optional)
rem Logs go to <root>\data\serve.log & daemon.log

rem resolve root = this script's directory
set "ROOT=%~dp0"
cd /d "%ROOT%"

rem already running? skip (idempotent, safe to call repeatedly)
tasklist /FI "IMAGENAME eq clipstream.exe" 2>NUL | find /I "clipstream.exe" >NUL
if %ERRORLEVEL%==0 goto daemon
start "clipstream-serve" /min bin\clipstream.exe serve >> data\serve.log 2>&1

:daemon
tasklist /FI "IMAGENAME eq clipstream-daemon.exe" 2>NUL | find /I "clipstream-daemon.exe" >NUL
if %ERRORLEVEL%==0 goto end
start "clipstream-daemon" /min bin\clipstream-daemon.exe -config config.yaml >> data\daemon.log 2>&1

:end
exit /b 0