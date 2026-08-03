#requires -Version 5
<#
  One-shot portable installer for Workass on Windows.

  Endpoint-security-friendly by design:
    - No MSI, no services, no drivers, no admin elevation.
    - No registry hive writes. The only optional system touch is a per-user
      Scheduled Task (lives in the user's own profile, not the machine hive).
    - Files are unblocked once (removes Mark-of-the-Web) so endpoint security
      does not re-scan every executable on each launch.
    - The signed upstream binaries (node.exe) are left byte-for-byte intact;
      nothing is renamed, rebranded, or patched, so nothing looks like a
      freshly modified EXE.

  Everything runs from a single user-writable folder. Delete the folder and
  the scheduled task and the machine is clean.

  Usage (from an elevated-or-normal PowerShell, both work):
    powershell -NoProfile -ExecutionPolicy Bypass -File .\Install-Workass.ps1
    powershell -NoProfile -ExecutionPolicy Bypass -File .\Install-Workass.ps1 -NoTask
    powershell -NoProfile -ExecutionPolicy Bypass -File .\Install-Workass.ps1 -InstallDir "$env:LOCALAPPDATA\Workass" -StartNow
#>
[CmdletBinding()]
param(
  # Where the portable app lives. Default is a user-writable, non-synced path.
  [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "Workass"),
  # Per-user Scheduled Task name for logon autostart. Empty/`-NoTask` skips it.
  [string]$TaskName = "Workass",
  [switch]$NoTask,
  [switch]$StartNow,
  # Optional explicit port override. When omitted, the daemon's own production
  # default applies (port 80 + LAN bind on Windows, per PORT-SPEC).
  [int]$Port = 0
)

$ErrorActionPreference = "Stop"

# The folder this script sits in is the staged bundle (workass.exe beside it).
$sourceDir = $PSScriptRoot
$sourceExe = Join-Path $sourceDir "workass.exe"
if (-not (Test-Path -LiteralPath $sourceExe -PathType Leaf)) {
  throw "workass.exe not found next to Install-Workass.ps1 ($sourceDir). Run this from the extracted bundle."
}

# If we are already running from the install dir, install in place; otherwise copy.
$alreadyInPlace = ([System.IO.Path]::GetFullPath($sourceDir).TrimEnd('\') -ieq `
                   [System.IO.Path]::GetFullPath($InstallDir).TrimEnd('\'))

if (-not $alreadyInPlace) {
  Write-Host "Installing Workass to $InstallDir ..."
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  # Copy the bundle. Existing files are overwritten so re-runs upgrade in place.
  Copy-Item -Path (Join-Path $sourceDir "*") -Destination $InstallDir -Recurse -Force
} else {
  Write-Host "Installing in place at $InstallDir ..."
}

$exe = Join-Path $InstallDir "workass.exe"
if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) {
  throw "Install failed: $exe not found after copy."
}

# Remove Mark-of-the-Web once so SmartScreen/EDR do not treat every file as a
# fresh internet download on each launch. Byte content is unchanged.
Write-Host "Unblocking files (one-time) ..."
Get-ChildItem -Path $InstallDir -Recurse -File -ErrorAction SilentlyContinue |
  Unblock-File -ErrorAction SilentlyContinue

# Optional per-user autostart. A Scheduled Task in the user's own context:
# no SYSTEM, no service control manager, no machine registry hive.
if (-not $NoTask -and $TaskName) {
  $stateDir = Join-Path $InstallDir "state"
  New-Item -ItemType Directory -Force -Path $stateDir | Out-Null
  $portArg = if ($Port -gt 0) { " --port $Port" } else { "" }
  $run = '"' + $exe + '" --prod --state-dir "' + $stateDir + '"' + $portArg
  $args = @(
    "/Create",
    "/TN", $TaskName,
    "/SC", "ONLOGON",
    "/RL", "LIMITED",     # least privilege: run as the user, never elevated
    "/TR", $run,
    "/F"
  )
  & schtasks.exe @args | Out-Null
  if ($LASTEXITCODE -ne 0) {
    Write-Warning "Scheduled task creation failed (exit $LASTEXITCODE). Continuing without autostart."
  } else {
    Write-Host "Registered per-user autostart task '$TaskName' (ONLOGON, limited)."
  }
}

Write-Host ""
Write-Host "Workass portable install complete."
Write-Host "  App:    $exe"
Write-Host "  Node:   $(Join-Path $InstallDir 'node\windows-amd64\node.exe')"
$effectivePort = if ($Port -gt 0) { $Port } else { 80 }
Write-Host "  Port:   $effectivePort (health: http://localhost:$effectivePort/workass/health)"
Write-Host ""
Write-Host "Launch now with:"
$launchArgs = "--prod --state-dir `"$(Join-Path $InstallDir 'state')`"" + $(if ($Port -gt 0) { " --port $Port" } else { "" })
Write-Host "  & `"$exe`" $launchArgs"

if ($StartNow) {
  Write-Host ""
  Write-Host "Starting Workass daemon ..."
  $daemonArgs = @("--prod", "--state-dir", (Join-Path $InstallDir "state"))
  if ($Port -gt 0) { $daemonArgs += @("--port", "$Port") }
  & $exe @daemonArgs
}
