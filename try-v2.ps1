# Try Houston 2.0 WITHOUT touching your v1 install.
#
#   - runs the v2 binary from target\release (your ~/.local/bin/houston.exe,
#     the v1 you use daily, is never replaced);
#   - points HOUSTON_HOME at a COPY of your store, so everything v2 writes
#     (config-v2.json, caches, last-use, the basics layout it provisions) lands
#     in the sandbox — your real ~/.claude/houston is untouched.
#
# Your real missions/accounts still show: missions are scanned from the
# untouched ~/.claude*/projects, and the copied accounts.json points at your
# real per-account dirs (reading them is fine; a quota probe may refresh a
# lapsed token there — the same refresh Claude/v1 already do — nothing else).
#
# Usage:  .\try-v2.ps1            (open the TUI)
#         .\try-v2.ps1 doctor     (or any verb)
#         .\try-v2.ps1 -Reset     (wipe the sandbox and re-copy the store)
[CmdletBinding()]
param([switch]$Reset, [Parameter(ValueFromRemainingArguments)] $Args)

$ErrorActionPreference = 'Stop'
$repo = Split-Path $MyInvocation.MyCommand.Path
$bin  = Join-Path $repo 'target\release\houston.exe'
if (-not (Test-Path $bin)) { throw "build it first:  cargo build --release  (in $repo)" }

$sandbox = Join-Path $env:TEMP 'houston2-sandbox'
$real    = Join-Path $env:USERPROFILE '.claude\houston'

if ($Reset -and (Test-Path $sandbox)) { Remove-Item -Recurse -Force $sandbox }
if (-not (Test-Path $sandbox)) {
    New-Item -ItemType Directory $sandbox | Out-Null
    if (Test-Path $real) { Copy-Item -Recurse -Force (Join-Path $real '*') $sandbox }
}

$env:HOUSTON_HOME = $sandbox
Write-Host "Houston 2.0 — isolated store: $sandbox" -ForegroundColor Cyan
Write-Host "(your v1 binary and real store are untouched)`n" -ForegroundColor DarkGray
& $bin @Args
