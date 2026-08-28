@echo off
setlocal
title Terminal Agent - Windows Service Setup
cd /d "%~dp0"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install-permanent-service.ps1"
echo.
pause
