# aisw installer for Windows (PowerShell):
#   irm https://raw.githubusercontent.com/yasinkuyu/ai-account-switcher/main/install.ps1 | iex
$ErrorActionPreference = 'Stop'

$repo = 'yasinkuyu/ai-account-switcher'
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$dir  = Join-Path $env:LOCALAPPDATA 'Programs\aisw'
New-Item -ItemType Directory -Force -Path $dir | Out-Null

Write-Host "Downloading aisw (windows/$arch)..."
Invoke-WebRequest -UseBasicParsing `
  -Uri "https://github.com/$repo/releases/latest/download/aisw-windows-$arch.exe" `
  -OutFile (Join-Path $dir 'aisw.exe')

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (($userPath -split ';') -notcontains $dir) {
  [Environment]::SetEnvironmentVariable('Path', "$userPath;$dir", 'User')
  Write-Host "Added $dir to your PATH. Open a new terminal to use it."
}
Write-Host "Installed: $dir\aisw.exe"
Write-Host "Run: aisw"
