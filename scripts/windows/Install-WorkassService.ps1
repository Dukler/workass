[CmdletBinding()]
param(
  [string]$ExePath = "C:\Program Files\Workass\workass.exe",
  [string]$StateDir = "C:\ProgramData\Workass\state",
  [string]$TaskName = "Workass Daemon",
  [switch]$StartNow
)

$ErrorActionPreference = "Stop"

# Endpoint-security-friendly install notes:
# - Do not rename the executable after validation; keep a stable path.
# - This wrapper uses Windows Task Scheduler instead of third-party service hosts.
# - The daemon is launched with --prod, so Windows defaults to port 80 and LAN bind
#   unless explicit --port/--bind arguments are added later.

$ResolvedExe = [System.IO.Path]::GetFullPath($ExePath)
$ResolvedState = [System.IO.Path]::GetFullPath($StateDir)

if (-not (Test-Path -LiteralPath $ResolvedExe -PathType Leaf)) {
  throw "Workass executable not found: $ResolvedExe"
}

New-Item -ItemType Directory -Force -Path $ResolvedState | Out-Null

$TaskRun = '"' + $ResolvedExe + '" --prod --state-dir "' + $ResolvedState + '"'
$Args = @(
  "/Create",
  "/TN", $TaskName,
  "/SC", "ONSTART",
  "/RL", "HIGHEST",
  "/RU", "SYSTEM",
  "/TR", $TaskRun,
  "/F"
)

& schtasks.exe @Args
if ($LASTEXITCODE -ne 0) {
  throw "schtasks.exe failed with exit code $LASTEXITCODE"
}

Write-Host "Installed scheduled task '$TaskName'"
Write-Host "Executable: $ResolvedExe"
Write-Host "State dir:  $ResolvedState"

if ($StartNow) {
  & schtasks.exe /Run /TN $TaskName
  if ($LASTEXITCODE -ne 0) {
    throw "schtasks.exe /Run failed with exit code $LASTEXITCODE"
  }
  Write-Host "Started scheduled task '$TaskName'"
}
