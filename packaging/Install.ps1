#requires -Version 7.0
<#
.SYNOPSIS
  Installer for Houston — mission-control for Claude Code: balanced multi-account
  launching (one CLAUDE_CONFIG_DIR per account, data shared via junction/symlink)
  plus a TUI to browse, organize and resume conversations.

.DESCRIPTION
  Installs the houston binary and prepares its data dir. Per-account setup (dirs,
  links, seed) is done afterwards by houston-setup-accounts.ps1. Flow:

    houston account add <label>      # register each account (just a label)
    houston-setup-accounts.ps1       # create per-account dirs + shared links
    houston run                      # launch; the first time each account /login's
    houston                          # browse / resume conversations

  Idempotent and cross-platform (Windows / macOS / Linux). Parameters exist
  mainly for testing against a sandbox.
#>
[CmdletBinding()]
param(
  [string]$BinDir,
  [switch]$NoProfileEdit,
  [string]$Version = 'latest',          # release tag to download (or 'latest')
  [string]$Repo    = 'edgarburgues/houston'
)

$ErrorActionPreference = 'Stop'
$pkg  = $PSScriptRoot
$repo = Split-Path -Parent $pkg   # repo layout: packaging/ -> root (zip: same dir)

function Info($m){ Write-Host $m -ForegroundColor Cyan }
function Ok($m){ Write-Host "  ✓ $m" -ForegroundColor Green }
function Warn($m){ Write-Host "  ! $m" -ForegroundColor Yellow }

if (-not $BinDir) { $BinDir = Join-Path $HOME '.local/bin' }
$storeDir = Join-Path $HOME '.claude/houston'   # must match accounts.StoreDir() in Go

function Get-Platform {
  $arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture) {
    'X64' { 'amd64' }; 'Arm64' { 'arm64' }; default { 'amd64' }
  }
  $os = if ($IsWindows) { 'windows' } elseif ($IsMacOS) { 'darwin' } else { 'linux' }
  "$os-$arch"
}

Info "Houston — installing"
Write-Host "  bin:   $BinDir"
Write-Host "  store: $storeDir"

# --- 1. binary ------------------------------------------------------------
# Priority: (a) local binary next to the script (zip distribution) ->
# (b) download from GitHub Releases verifying SHA-256 -> (c) build with Go.
Info "1) houston binary"
$exe   = if ($IsWindows) { 'houston.exe' } else { 'houston' }
$plat  = Get-Platform
$asset = if ($IsWindows) { "houston-$plat.exe" } else { "houston-$plat" }
New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
$binDst   = Join-Path $BinDir $exe
$prebuilt = Join-Path $pkg "bin/$plat/$exe"

if (Test-Path $prebuilt) {
  # (a) local binary (zip)
  Copy-Item $prebuilt $binDst -Force
  if (-not $IsWindows) { chmod +x $binDst }
  Ok "houston ($plat) -> $binDst (local)"
} else {
  # (b) download + checksum verification
  $base = if ($Version -eq 'latest') {
    "https://github.com/$Repo/releases/latest/download"
  } else {
    "https://github.com/$Repo/releases/download/$Version"
  }
  $downloaded = $false
  try {
    $tmp  = Join-Path ([IO.Path]::GetTempPath()) $asset
    $sums = Join-Path ([IO.Path]::GetTempPath()) 'houston-checksums.txt'
    Warn "downloading $asset from Releases ($Version)..."
    Invoke-WebRequest -Uri "$base/$asset"        -OutFile $tmp  -UseBasicParsing
    Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile $sums -UseBasicParsing
    $line = Select-String -Path $sums -Pattern ([regex]::Escape($asset) + '\s*$') | Select-Object -First 1
    if (-not $line) { throw "couldn't find $asset in checksums.txt" }
    $expected = ($line.Line -split '\s+')[0]
    $actual   = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash
    if ($actual -ine $expected) {
      throw "checksum MISMATCH for ${asset}: expected $expected, got $actual"
    }
    Copy-Item $tmp $binDst -Force
    if (-not $IsWindows) { chmod +x $binDst }
    Ok "houston ($plat) -> $binDst (release $Version, SHA-256 verified)"
    $downloaded = $true
  } catch {
    Warn "download/verification failed: $($_.Exception.Message)"
  }
  if (-not $downloaded) {
    # (c) build from source
    if (Get-Command go -ErrorAction SilentlyContinue) {
      Warn "building with go..."
      Push-Location $repo
      try {
        & go build -o $binDst .
        if ($LASTEXITCODE -ne 0 -or -not (Test-Path $binDst)) { throw "go build failed" }
        Ok "built -> $binDst"
      } finally { Pop-Location }
    } else {
      throw "couldn't download the binary for $plat and there's no Go to build with; download the Releases zip or install Go"
    }
  }
}

# --- 2. data dir + setup script -------------------------------------------
Info "2) Data dir and setup script"
New-Item -ItemType Directory -Path $storeDir -Force | Out-Null
Ok $storeDir
$setupSrc = @((Join-Path $pkg 'houston-setup-accounts.ps1'), (Join-Path $repo 'houston-setup-accounts.ps1')) |
  Where-Object { Test-Path $_ } | Select-Object -First 1
if ($setupSrc) {
  Copy-Item $setupSrc (Join-Path $storeDir 'setup-accounts.ps1') -Force
  Ok "setup-accounts.ps1 -> $storeDir"
}

# --- 3. PATH + alias claude -----------------------------------------------
if (-not $NoProfileEdit) {
  Info "3) PATH + alias claude -> houston run"
  $prof = $PROFILE.CurrentUserAllHosts
  $profDir = Split-Path $prof -Parent
  if (-not (Test-Path $profDir)) { New-Item -ItemType Directory -Path $profDir -Force | Out-Null }
  $profText = if (Test-Path $prof) { Get-Content $prof -Raw } else { '' }
  $marker = '# >>> houston >>>'
  if ($profText -notmatch [regex]::Escape($marker)) {
    $block = @"
$marker
if ((`$env:PATH -split [IO.Path]::PathSeparator) -notcontains '$BinDir') { `$env:PATH = '$BinDir' + [IO.Path]::PathSeparator + `$env:PATH }
# <<< houston <<<
"@
    Add-Content -Path $prof -Value "`n$block`n"
    Ok "profile updated ($prof)"
  } else { Ok "profile already configured" }

  # `claude` -> `houston run` alias: `claude ...` feels normal while Houston
  # orchestrates it (picks an account, sets CLAUDE_CONFIG_DIR, launches the real claude).
  # It is a shell function, so it does not clash with the claude.exe on the PATH
  # (Houston resolves it by absolute path in its child process).
  $profText = if (Test-Path $prof) { Get-Content $prof -Raw } else { '' }
  $cmarker = '# >>> houston-claude >>>'
  if ($profText -notmatch [regex]::Escape($cmarker)) {
    $cblock = @'
# >>> houston-claude >>>
# `claude ...` is routed through Houston. To search/resume conversations: houston
function claude { houston run @args }
# <<< houston-claude <<<
'@
    Add-Content -Path $prof -Value "`n$cblock`n"
    Ok "claude -> houston run alias added"
  } else { Ok "claude alias already configured" }

  # Windows: also persist to the user environment so cmd/GUI sessions find it.
  if ($IsWindows) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($userPath -split ';') -notcontains $BinDir) {
      [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $BinDir), 'User')
      Ok "user PATH (Windows) updated"
    }
  }
}

Write-Host ""
Info "Done. Next steps (in a new terminal):"
Write-Host "  1) register each account:  houston account add <label>"
Write-Host "  2) create dirs + links:    pwsh `"$storeDir\setup-accounts.ps1`""
Write-Host "  3) launch (1st-time login): houston run   (or simply: claude)"
Write-Host "  4) manage/resume:          houston"
Write-Host ""
Write-Host "  'claude ...' is routed through Houston (= 'houston run ...'). To search/resume: houston." -ForegroundColor DarkGray
Write-Host "  Each account has its own login; the data (projects/sessions/…) is shared." -ForegroundColor DarkGray
