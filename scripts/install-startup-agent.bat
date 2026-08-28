@echo off
setlocal
title Terminal Agent - Auto-Start Setup
cd /d "%~dp0"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install-startup-agent.ps1"
echo.
pause
