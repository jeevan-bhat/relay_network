[CmdletBinding()]
param()

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "  Terminal Agent - Permanent Windows Service Setup" -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host ""

# 1. Verify Admin
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Write-Host "[!] Administrator privileges required. Elevating..." -ForegroundColor Yellow
    Start-Process powershell.exe -Verb RunAs -ArgumentList ("-NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`"")
    exit
}

$agentDir = "C:\ProgramData\TerminalAgent"
if (-not (Test-Path $agentDir)) {
    New-Item -ItemType Directory -Path $agentDir -Force | Out-Null
}

# 2. Locate or Build terminal-agent.exe
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
if ([string]::IsNullOrWhiteSpace($scriptDir)) {
    $scriptDir = $PSScriptRoot
}
$srcExe = Join-Path $scriptDir "..\agent\terminal-agent.exe"

if (-not (Test-Path $srcExe)) {
    Write-Host "Compiling agent binary..." -ForegroundColor Yellow
    Push-Location (Join-Path $scriptDir "..\agent")
    go build -o terminal-agent.exe ./cmd/terminal-agent
    Pop-Location
}

if (-not (Test-Path $srcExe)) {
    Write-Host "Error: Could not locate terminal-agent.exe. Please build it first." -ForegroundColor Red
    Write-Host "Press any key to exit..."
    $null = [Console]::ReadKey()
    exit 1
}

# 3. Prompt for URL & Device ID
Write-Host "Configure Cloud Connection:" -ForegroundColor White
Write-Host "----------------------------------------------------------" -ForegroundColor Gray
$relayURL = Read-Host "Enter your Cloud Relay URL (e.g. wss://your-relay.onrender.com/ws)"
while ([string]::IsNullOrWhiteSpace($relayURL)) {
    $relayURL = Read-Host "URL cannot be empty. Please enter your Cloud Relay URL"
}

# Clean input: ensure wss:// prefix and /ws suffix
$relayURL = $relayURL.Trim()
if ($relayURL.StartsWith("https://")) {
    $relayURL = "wss://" + $relayURL.Substring(8)
} elseif (-not $relayURL.StartsWith("wss://") -and -not $relayURL.StartsWith("ws://")) {
    $relayURL = "wss://" + $relayURL
}
if (-not $relayURL.EndsWith("/ws")) {
    $relayURL = $relayURL.TrimEnd('/') + "/ws"
}

$defaultDevice = $env:COMPUTERNAME.ToLower()
$deviceID = Read-Host "Enter Device ID [Press Enter for '$defaultDevice']"
if ([string]::IsNullOrWhiteSpace($deviceID)) {
    $deviceID = $defaultDevice
}
$deviceID = $deviceID.Trim()

$existingToken = ""
if (Test-Path "C:\ProgramData\TerminalAgent\config.json") {
    try {
        $prev = Get-Content "C:\ProgramData\TerminalAgent\config.json" -Raw | ConvertFrom-Json
        if ($prev.auth_token) { $existingToken = $prev.auth_token }
    } catch {}
}

$authToken = Read-Host "Enter Account Pairing Token [Press Enter for '$existingToken']"
if ([string]::IsNullOrWhiteSpace($authToken)) {
    $authToken = $existingToken
}
$authToken = $authToken.Trim()

Write-Host ""
Write-Host "Connecting device '$deviceID' to '$relayURL'..." -ForegroundColor Cyan

# 4. Write Persistent Config
$configObj = [PSCustomObject]@{
    relay_url          = $relayURL
    device_id          = $deviceID
    auth_token         = $authToken
    db_path            = "C:\ProgramData\TerminalAgent\queue.db"
    heartbeat_interval = "15s"
}
$configPath = Join-Path $agentDir "config.json"
$configObj | ConvertTo-Json -Depth 4 | Set-Content -Path $configPath -Force -Encoding UTF8
Write-Host "[OK] Configuration saved to $configPath" -ForegroundColor Green

# 5. Stop and uninstall any old service version
Stop-Process -Name "terminal-agent" -Force -ErrorAction SilentlyContinue
if (Get-Service -Name "TerminalAgent" -ErrorAction SilentlyContinue) {
    Write-Host "Updating existing TerminalAgent service..." -ForegroundColor Gray
    Stop-Service -Name "TerminalAgent" -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 1
    sc.exe delete TerminalAgent | Out-Null
    Start-Sleep -Seconds 1
}

# 6. Copy binary to ProgramData
$destExe = Join-Path $agentDir "terminal-agent.exe"
Copy-Item -Path $srcExe -Destination $destExe -Force
Write-Host "[OK] Installed executable to $destExe" -ForegroundColor Green

# 7. Register and Start Service via Windows SCM
Write-Host "Installing Windows Service (Automatic Startup on Boot)..." -ForegroundColor Yellow

try {
    Start-Process -FilePath "$destExe" -ArgumentList "install" -Wait -NoNewWindow -ErrorAction SilentlyContinue
} catch {}

# Fallback: create directly via sc.exe if not registered
$svcCheck = Get-Service -Name "TerminalAgent" -ErrorAction SilentlyContinue
if (-not $svcCheck) {
    sc.exe create TerminalAgent binPath= "`"$destExe`"" start= auto DisplayName= "Terminal Agent" | Out-Null
}

sc.exe config TerminalAgent start= auto | Out-Null

try {
    Start-Process -FilePath "$destExe" -ArgumentList "start" -Wait -NoNewWindow -ErrorAction SilentlyContinue
} catch {}
Start-Service -Name "TerminalAgent" -ErrorAction SilentlyContinue

Start-Sleep -Seconds 2

$svc = Get-Service -Name "TerminalAgent" -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -eq "Running") {
    Write-Host ""
    Write-Host "==========================================================" -ForegroundColor Green
    Write-Host "  SUCCESS! Terminal Agent is now permanently running." -ForegroundColor Green
    Write-Host "==========================================================" -ForegroundColor Green
    Write-Host "  It will start automatically every time your laptop powers on." -ForegroundColor White
    Write-Host "  You NEVER need to open a terminal or run a command again." -ForegroundColor White
    Write-Host "  Cloud Relay: $relayURL" -ForegroundColor Cyan
    Write-Host "  Device ID:   $deviceID" -ForegroundColor Cyan
    Write-Host "==========================================================" -ForegroundColor Green
} else {
    Write-Host "Service status: $($svc.Status)" -ForegroundColor Yellow
}

Write-Host ""
Write-Host "Setup complete." -ForegroundColor Gray
