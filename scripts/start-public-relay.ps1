# Start Relay Server with Public Tunnel for Remote Multi-Network Access
# Allows mobile phone (on 4G/5G or any Wi-Fi) to control laptop from anywhere.

Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "  Terminal App - Remote Multi-Network Public Launcher" -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host ""

# 1. Start Relay Server in background
$relayPath = Join-Path $PSScriptRoot "..\relay\terminal-relay.exe"
if (-not (Test-Path $relayPath)) {
    Write-Host "Building terminal-relay.exe..." -ForegroundColor Yellow
    Push-Location (Join-Path $PSScriptRoot "..\relay")
    go build -o terminal-relay.exe ./cmd/terminal-relay
    Pop-Location
}

Write-Host "[1/2] Starting Relay Server on port 8080..." -ForegroundColor Green
$relayProcess = Start-Process -FilePath $relayPath -ArgumentList "--port 8080 --db ./data/relay.db" -PassThru -NoNewWindow

Start-Sleep -Seconds 1

Write-Host ""
Write-Host "[2/2] Exposing Relay to Public Internet (Global NAT Traversal)..." -ForegroundColor Green
Write-Host "Checking for cloudflared / ngrok / npx localtunnel..." -ForegroundColor Gray

if (Get-Command "cloudflared" -ErrorAction SilentlyContinue) {
    Write-Host "Starting Cloudflare Quick Tunnel (Free, No account needed)..." -ForegroundColor Cyan
    Write-Host "Open the generated https://*.trycloudflare.com URL on your mobile phone!" -ForegroundColor Yellow
    cloudflared tunnel --url http://localhost:8080
} elseif (Get-Command "ngrok" -ErrorAction SilentlyContinue) {
    Write-Host "Starting ngrok tunnel on port 8080..." -ForegroundColor Cyan
    ngrok http 8080
} elseif (Get-Command "npx" -ErrorAction SilentlyContinue) {
    Write-Host "Starting localtunnel via npx..." -ForegroundColor Cyan
    npx localtunnel --port 8080
} else {
    Write-Host ""
    Write-Host "--------------------------------------------------------" -ForegroundColor Yellow
    Write-Host "Local Relay is running on http://localhost:8080" -ForegroundColor Green
    Write-Host ""
    Write-Host "To access from a DIFFERENT network (e.g. 4G/5G mobile):" -ForegroundColor Yellow
    Write-Host "Option A (Instant 1-command tunnel - Recommended):" -ForegroundColor White
    Write-Host "  Run: winget install Cloudflare.cloudflared" -ForegroundColor Cyan
    Write-Host "  Then: cloudflared tunnel --url http://localhost:8080" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "Option B (Free Cloud Hosting):" -ForegroundColor White
    Write-Host "  Deploy relay/Dockerfile to Render.com or Fly.io (Free 24/7)" -ForegroundColor Cyan
    Write-Host "--------------------------------------------------------" -ForegroundColor Yellow
    
    $relayProcess.WaitForExit()
}
