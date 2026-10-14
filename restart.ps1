# Spotlight - Restart script (Backend + Frontend)
Write-Host "===================================================" -ForegroundColor Cyan
Write-Host "  Spotlight - Restart Backend + Frontend" -ForegroundColor Cyan
Write-Host "===================================================" -ForegroundColor Cyan

Write-Host "[1/4] Stopping previous instances..." -ForegroundColor Yellow
Stop-Process -Name "SpotlightUI", "spotlight-backend" -Force -ErrorAction SilentlyContinue
Get-NetTCPConnection -LocalPort 9876, 49152 -ErrorAction SilentlyContinue | ForEach-Object {
    Stop-Process -Id $_.OwningProcess -Force -ErrorAction SilentlyContinue
}
Start-Sleep -Milliseconds 400

$root = $PSScriptRoot
if (-not $root) { $root = (Get-Location).Path }

Write-Host "[2/4] Verifying and building..." -ForegroundColor Yellow
Push-Location "$root\spotlight-backend"
go build -ldflags "-H=windowsgui" -o spotlight-backend.exe
Pop-Location

Push-Location "$root\SpotlightUI"
dotnet build -c Release -v q --nologo
Pop-Location

Write-Host "[3/4] Starting Backend in background..." -ForegroundColor Green
([wmiclass]"Win32_Process").Create("$root\spotlight-backend\spotlight-backend.exe", "$root\spotlight-backend", $null) | Out-Null

Start-Sleep -Milliseconds 600

Write-Host "[4/4] Starting Frontend Spotlight UI..." -ForegroundColor Green
([wmiclass]"Win32_Process").Create("$root\SpotlightUI\bin\Release\net10.0-windows\SpotlightUI.exe", "$root\SpotlightUI\bin\Release\net10.0-windows", $null) | Out-Null

Write-Host "`nReady! Spotlight is running. Press [Alt + Space] to search." -ForegroundColor Cyan
