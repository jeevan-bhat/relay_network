@echo off
setlocal
cd /d "%~dp0"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install-startup-agent.ps1"
if %errorlevel% neq 0 (
    echo.
    echo Setup encountered an issue.
    pause
)
