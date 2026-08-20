# Install the freshly built binary as the daily `houston`, WITHOUT killing a
# running one.
#
# Why this script exists: the obvious install is `Copy-Item` over
# ~/.local/bin/houston.exe, which Windows refuses while the file is executing —
# so the obvious fix is to kill the running Houston first. That is wrong, and it
# cost a real session: killing the TUI leaves the console in raw mode with mouse
# capture on (moving the pointer then types escape sequences) and orphans whatever
# `claude` it had launched.
#
# Houston's own self-update already solved this (see update::swap): on Windows you
# cannot overwrite a running exe, but you CAN rename it. So move the old one aside
# and copy the new one into place. The running process keeps executing from the
# renamed file, finishes whenever it finishes, and the next `houston` start
# collects the leftovers via update::cleanup_stale.
#
#   pwsh -File install-local.ps1            # build + install
#   pwsh -File install-local.ps1 -NoBuild   # install what is already built

param([switch]$NoBuild)

$ErrorActionPreference = 'Stop'
$repo = $PSScriptRoot
$exe = Join-Path $repo 'target\release\houston.exe'
$dest = Join-Path $env:USERPROFILE '.local\bin\houston.exe'

if (-not $NoBuild) {
    $env:PATH = "$env:USERPROFILE\.cargo\bin;$env:PATH"
    Push-Location $repo
    try { cargo build --release -p houston } finally { Pop-Location }
}
if (-not (Test-Path $exe)) { throw "not built: $exe" }

$running = @(Get-Process houston -ErrorAction SilentlyContinue).Count
try {
    Copy-Item $exe $dest -Force -ErrorAction Stop
    $how = 'copied over'
} catch [System.IO.IOException] {
    # Locked, so it is running. Rename it aside — permitted even while executing —
    # and drop the new one in its place.
    $aside = "$dest.old-{0}" -f (Get-Date -Format 'yyyyMMddHHmmss')
    Move-Item $dest $aside -Force
    Copy-Item $exe $dest -Force
    $how = "installed alongside $running running instance(s); old binary parked at $(Split-Path $aside -Leaf)"
}

# Sweep any parked binaries that are no longer locked, the same housekeeping
# `houston` does for itself at startup.
Get-ChildItem (Split-Path $dest) -Filter 'houston.exe.old-*' -ErrorAction SilentlyContinue | ForEach-Object {
    try { Remove-Item $_.FullName -Force -ErrorAction Stop } catch { }
}

"$how"
"version: " + (& $dest --version)
"note: a running Houston keeps the OLD build until you close and reopen it."
