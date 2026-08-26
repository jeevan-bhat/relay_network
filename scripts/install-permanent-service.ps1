# 1-Click Permanent Windows Background Service Installer for Terminal Agent
# Runs automatically in the background on laptop boot (before login) without opening terminals.

# Require Administrator
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Host "Elevating permissions to Administrator..." -ForegroundColor Yellow
    Start-Process powershell.exe -Verb RunAs -ArgumentList ("-NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`"")
    exit
}

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "  Terminal Agent - Permanent Windows Service Installer" -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host ""

$agentDir = "C:\ProgramData\TerminalAgent"
if (-not (Test-Path $agentDir)) {
    New-Item -ItemType Directory -Path $agentDir -Force | Out-Null
}

# 1. Build or Copy terminal-agent.exe
$srcExe = Join-Path $PSScriptRoot "..\agent\terminal-agent.exe"
if (-not (Test-Path $srcExe)) {
    Write-Host "Building terminal-agent.exe..." -ForegroundColor Yellow
    Push-Location (Join-Path $PSScriptRoot "..\agent")
    go build -o terminal-agent.exe ./cmd/terminal-agent
    Pop-Location
}

$destExe = Join-Path $agentDir "terminal-agent.exe"
Copy-Item -Path $srcExe -Destination $destExe -Force
Write-Host "[✓] Installed executable to $destExe" -ForegroundColor Green

# 2. Get Relay Cloud URL & Device ID
Write-Host ""
$defaultRelay = "wss://my-relay.onrender.com/ws"
$relayURL = Read-Host "Enter your Cloud Relay URL (e.g. wss://your-app.onrender.com/ws) [Press Enter for $defaultRelay]"
if ([string]::IsNullOrWhiteSpace($relayURL)) {
    $relayURL = $defaultRelay
}

$defaultDevice = $env:COMPUTERNAME.ToLower()
$deviceID = Read-Host "Enter Device ID [Press Enter for $defaultDevice]"
if ([string]::IsNullOrWhiteSpace($deviceID)) {
    $deviceID = $defaultDevice
}

# 3. Create Persistent Config
$configJson = @"
{
    "relay_url": "$relayURL",
    "device_id": "$deviceID",
    "db_path": "$agentDir\\queue.db",
    "heartbeat_interval": "15s"
}
"@
$configPath = Join-Path $agentDir "config.json"
Set-Content -Path $configPath -Value $configJson -Force
Write-Host "[✓] Saved configuration to $configPath" -ForegroundColor Green

# 4. Stop existing service if present
if (Get-Service -Name "TerminalAgent" -ErrorAction SilentlyContinue) {
    Write-Host "Stopping existing service..." -ForegroundColor Gray
    Stop-Service -Name "TerminalAgent" -Force -ErrorAction SilentlyContinue
    & $destExe uninstall
    Start-Sleep -Seconds 1
}

# 5. Install and start as Automatic Windows Service
Write-Host "Installing Windows Service (Automatic Startup on Boot)..." -ForegroundColor Yellow
& $destExe install --relay $relayURL --device-id $deviceID --db "$agentDir\queue.db"
& $destExe start

Start-Sleep -Seconds 2

$svc = Get-Service -Name "TerminalAgent" -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -eq "Running") {
    Write-Host ""
    Write-Host "==========================================================" -ForegroundColor Green
    Write-Host "  SUCCESS! Terminal Agent is now permanently running." -ForegroundColor Green
    Write-Host "==========================================================" -ForegroundColor Green
    Write-Host "• It will start automatically every time your laptop turns on." -ForegroundColor White
    Write-Host "• You NEVER need to open a terminal or run a command again." -ForegroundColor White
    Write-Host "• Connects automatically to: $relayURL" -ForegroundColor Cyan
    Write-Host "• Device ID: $deviceID" -ForegroundColor Cyan
    Write-Host "==========================================================" -ForegroundColor Green
} else {
    Write-Host "Service installed. Status: $($svc.Status)" -ForegroundColor Yellow
}

Write-Host ""
Write-Host "Press any key to exit..." -ForegroundColor Gray
$null = $Host.UI.RawUI.ReadKey("NoEcho,IncludeKeyDown")
