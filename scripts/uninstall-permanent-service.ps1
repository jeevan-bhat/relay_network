# Uninstall Terminal Agent Windows Service
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) {
    Start-Process powershell.exe -Verb RunAs -ArgumentList ("-NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`"")
    exit
}

$destExe = "C:\ProgramData\TerminalAgent\terminal-agent.exe"
if (Test-Path $destExe) {
    & $destExe stop
    & $destExe uninstall
}

Stop-Service -Name "TerminalAgent" -Force -ErrorAction SilentlyContinue
sc.exe delete TerminalAgent | Out-Null
Write-Host "TerminalAgent service uninstalled successfully." -ForegroundColor Green
Start-Sleep -Seconds 2
