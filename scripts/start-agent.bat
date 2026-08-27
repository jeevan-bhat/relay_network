@echo off
setlocal
title Terminal Agent (Live Console)
cd /d "%~dp0..\agent"

echo ==========================================================
echo   Terminal Agent - Live Foreground Runner
echo ==========================================================
echo.

if not exist "terminal-agent.exe" (
    echo Building terminal-agent.exe...
    go build -o terminal-agent.exe ./cmd/terminal-agent
)

echo Starting agent... Press Ctrl+C to stop.
echo.
"%~dp0..\agent\terminal-agent.exe" run
if %errorlevel% neq 0 (
    echo.
    echo Agent stopped.
    pause
)
