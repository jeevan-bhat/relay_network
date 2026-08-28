# 1-Click Permanent Background Startup Installer (Zero-Admin Required)
# Automatically starts Terminal Agent invisibly in the background every time you turn on your laptop.

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "  Terminal Agent - Permanent Auto-Start Setup (Zero-Admin)" -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host ""

$agentDir = "$env:LOCALAPPDATA\TerminalAgent"
if (-not (Test-Path $agentDir)) {
    New-Item -ItemType Directory -Path $agentDir -Force | Out-Null
}

# 1. Locate or Copy binary
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

$destExe = Join-Path $agentDir "terminal-agent.exe"
Stop-Process -Name "terminal-agent" -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 500
Copy-Item -Path $srcExe -Destination $destExe -Force
Write-Host "[OK] Agent binary placed at: $destExe" -ForegroundColor Green

# 2. Check existing config or prompt
$configPath = Join-Path $agentDir "config.json"
$existingRelay = "wss://relay-network.onrender.com/ws"
$existingDevice = $env:COMPUTERNAME.ToLower()
$existingToken = ""

if (Test-Path "C:\ProgramData\TerminalAgent\config.json") {
    try {
        $prev = Get-Content "C:\ProgramData\TerminalAgent\config.json" -Raw | ConvertFrom-Json
        if ($prev.relay_url) { $existingRelay = $prev.relay_url }
        if ($prev.device_id) { $existingDevice = $prev.device_id }
        if ($prev.auth_token) { $existingToken = $prev.auth_token }
    } catch {}
}

Write-Host ""
Write-Host "Configure Cloud Connection:" -ForegroundColor White
Write-Host "----------------------------------------------------------" -ForegroundColor Gray
$relayURL = Read-Host "Enter your Cloud Relay URL [Press Enter for '$existingRelay']"
if ([string]::IsNullOrWhiteSpace($relayURL)) {
    $relayURL = $existingRelay
}

# Format URL cleanly
$relayURL = $relayURL.Trim()
if ($relayURL.StartsWith("https://")) {
    $relayURL = "wss://" + $relayURL.Substring(8)
} elseif (-not $relayURL.StartsWith("wss://") -and -not $relayURL.StartsWith("ws://")) {
    $relayURL = "wss://" + $relayURL
}
if (-not $relayURL.EndsWith("/ws")) {
    $relayURL = $relayURL.TrimEnd('/') + "/ws"
}

$deviceID = Read-Host "Enter Device ID [Press Enter for '$existingDevice']"
if ([string]::IsNullOrWhiteSpace($deviceID)) {
    $deviceID = $existingDevice
}
$deviceID = $deviceID.Trim()

$authToken = Read-Host "Enter Account Pairing Token [Press Enter for '$existingToken']"
if ([string]::IsNullOrWhiteSpace($authToken)) {
    $authToken = $existingToken
}
$authToken = $authToken.Trim()

# 3. Save Config
$configObj = [PSCustomObject]@{
    relay_url          = $relayURL
    device_id          = $deviceID
    auth_token         = $authToken
    db_path            = (Join-Path $agentDir "queue.db")
    heartbeat_interval = "15s"
}
$configObj | ConvertTo-Json -Depth 4 | Set-Content -Path $configPath -Force -Encoding UTF8
Write-Host "[OK] Configuration saved to $configPath" -ForegroundColor Green

# Also save to ProgramData if accessible
try {
    if (-not (Test-Path "C:\ProgramData\TerminalAgent")) {
        New-Item -ItemType Directory -Path "C:\ProgramData\TerminalAgent" -Force -ErrorAction SilentlyContinue | Out-Null
    }
    $configObj | ConvertTo-Json -Depth 4 | Set-Content -Path "C:\ProgramData\TerminalAgent\config.json" -Force -Encoding UTF8 -ErrorAction SilentlyContinue
} catch {}

# 4. Create Invisible Startup Launcher in Windows Startup Folder
$startupFolder = [Environment]::GetFolderPath("Startup")
$vbsPath = Join-Path $startupFolder "TerminalAgent.vbs"
$line1 = 'Set WshShell = CreateObject("WScript.Shell")'
$line2 = 'WshShell.Run """{0}"" run", 0, False' -f $destExe
Set-Content -Path $vbsPath -Value @($line1, $line2) -Force
Write-Host "[OK] Registered in Windows Startup: $vbsPath" -ForegroundColor Green

# 5. Launch immediately in background
Write-Host "Starting agent in background..." -ForegroundColor Yellow
Start-Process -FilePath "wscript.exe" -ArgumentList $vbsPath

Start-Sleep -Seconds 2

$proc = Get-Process -Name "terminal-agent" -ErrorAction SilentlyContinue
if ($proc) {
    Write-Host ""
    Write-Host "==========================================================" -ForegroundColor Green
    Write-Host "  SUCCESS! Terminal Agent is now permanently running." -ForegroundColor Green
    Write-Host "==========================================================" -ForegroundColor Green
    Write-Host "  It will start automatically in background on every boot." -ForegroundColor White
    Write-Host "  No terminal window or popups will ever appear." -ForegroundColor White
    Write-Host "  Connected to: $relayURL" -ForegroundColor Cyan
    Write-Host "  Device ID:   $deviceID" -ForegroundColor Cyan
    Write-Host "==========================================================" -ForegroundColor Green
} else {
    Write-Host "Agent installed to startup. To run immediately, open scripts\start-agent.bat." -ForegroundColor Yellow
}

Write-Host ""
Write-Host "Setup complete." -ForegroundColor Gray
