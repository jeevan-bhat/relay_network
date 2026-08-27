@echo off
setlocal
cd /d "%~dp0"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install-permanent-service.ps1"
if %errorlevel% neq 0 (
    echo.
    echo An error occurred during setup.
    pause
)
