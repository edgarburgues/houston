#requires -Version 7.0
<#
.SYNOPSIS
  Installer for Houston — mission-control for Claude Code: balanced multi-account
  launching (one CLAUDE_CONFIG_DIR per account, data shared via junction/symlink)
  plus a TUI to browse, organize and resume conversations.

.DESCRIPTION
  Installs the houston binary and prepares its data dir. Per-account setup (dirs,
  links, seed) is done by the Rust account commands. Flow:

    houston account add <label>      # register each account (just a label)
    houston doctor --fix            # repair per-account dirs and shared links
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
$repoRoot = Split-Path -Parent $pkg   # repo layout: packaging/ -> root (zip: same dir)

function Info($m){ Write-Host $m -ForegroundColor Cyan }
function Ok($m){ Write-Host "  ✓ $m" -ForegroundColor Green }
function Warn($m){ Write-Host "  ! $m" -ForegroundColor Yellow }

if (-not $BinDir) { $BinDir = Join-Path $HOME '.local/bin' }
$storeDir = $(if ($env:HOUSTON_HOME) { $env:HOUSTON_HOME } else { Join-Path $HOME '.claude/houston' })   # matches houston_core::paths::store_dir

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
# (b) signed GitHub Release, checked by an independent minisign -> (c) local Rust source.
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
  # (b) Verify with an already trusted minisign, never the downloaded executable.
  $downloaded = $false
  $verifying = $false
  $tmp = $sums = $signature = $null
  try {
    if (-not (Get-Command minisign -ErrorAction SilentlyContinue)) {
      throw 'minisign is required to authenticate release downloads; using local source if available'
    }
    $tag = $Version
    if ($tag -eq 'latest') {
      $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest"
      $tag = $release.tag_name
    }
    if ($tag -notmatch '^v2\.\d+\.\d+(?:[-+][A-Za-z0-9.-]+)?$') { throw "unsupported release tag: $tag" }
    $base = "https://github.com/$Repo/releases/download/$tag"

    $tmp  = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString('N') + '-' + $asset)
    $sums = Join-Path ([IO.Path]::GetTempPath()) ('houston-checksums-' + [guid]::NewGuid().ToString('N') + '.txt')
    $signature = "$sums.minisig"
    Warn "downloading $asset from Releases ($Version)..."
    Invoke-WebRequest -Uri "$base/$asset"        -OutFile $tmp  -UseBasicParsing
    Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile $sums -UseBasicParsing
    Invoke-WebRequest -Uri "$base/checksums.txt.minisig" -OutFile $signature -UseBasicParsing
    $verifying = $true
    # Public key pinned independently of the release. Forks may explicitly override it.
    $pubkey = if ($env:HOUSTON_UPDATE_PUBKEY) { $env:HOUSTON_UPDATE_PUBKEY } else { 'RWQIQDB05FRJEgi1W8tFHFrqNC337cPTDF3QwORxXkLFJ37MJ2Bw60Wi' }
    & minisign -Vm $sums -x $signature -P $pubkey
    if ($LASTEXITCODE -ne 0) { throw 'release signature verification failed' }
    $trusted = @(Get-Content -LiteralPath $signature | Where-Object { $_.StartsWith('trusted comment: ') })
    if ($trusted.Count -ne 1 -or (($trusted[0] -replace '^trusted comment: ', '') -split '\s+') -cnotcontains "tag:$tag") {
      throw 'signed checksums do not belong to the requested release'
    }
    $line = Select-String -Path $sums -Pattern ([regex]::Escape($asset) + '\s*$') | Select-Object -First 1
    if (-not $line) { throw "couldn't find $asset in checksums.txt" }
    $expected = ($line.Line -split '\s+')[0]
    $actual   = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash
    if ($actual -ine $expected) {
      throw "checksum MISMATCH for ${asset}: expected $expected, got $actual"
    }
    Copy-Item $tmp $binDst -Force
    if (-not $IsWindows) { chmod +x $binDst }
    Ok "houston ($plat) -> $binDst (release $tag, signature and SHA-256 verified)"
    $downloaded = $true
  } catch {
    if ($verifying) { throw } # Authentication failures never install or silently fall back.
    Warn "download/verification failed: $($_.Exception.Message)"
  }
  finally {
    if ($tmp -and (Test-Path -LiteralPath $tmp)) { Remove-Item -LiteralPath $tmp -Force }
    if ($signature -and (Test-Path -LiteralPath $signature)) { Remove-Item -LiteralPath $signature -Force }
    if ($sums -and (Test-Path -LiteralPath $sums)) { Remove-Item -LiteralPath $sums -Force }
  }
  if (-not $downloaded) {
    # (c) build from source
    if ((Test-Path -LiteralPath (Join-Path $repoRoot 'Cargo.toml')) -and (Get-Command cargo -ErrorAction SilentlyContinue)) {
      Warn "building with cargo..."
      Push-Location $repoRoot
      try {
        & cargo build --locked --release -p houston
        if ($LASTEXITCODE -ne 0) { throw "cargo build failed" }
        Copy-Item (Join-Path $repoRoot "target/release/$exe") $binDst -Force
        if ($LASTEXITCODE -ne 0 -or -not (Test-Path $binDst)) { throw "cargo build failed" }
        Ok "built -> $binDst"
      } finally { Pop-Location }
    } else {
      throw "couldn't download the binary for $plat and there's no Rust toolchain to build with; download the Releases zip or install Rust"
    }
  }
}

# --- 2. data dir ----------------------------------------------------------
Info "2) Data dir"
New-Item -ItemType Directory -Path $storeDir -Force | Out-Null
Ok $storeDir

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
Write-Host "  2) inspect setup:          houston doctor"
Write-Host "  3) launch (1st-time login): houston run   (or simply: claude)"
Write-Host "  4) manage/resume:          houston"
Write-Host ""
Write-Host "  'claude ...' is routed through Houston (= 'houston run ...'). To search/resume: houston." -ForegroundColor DarkGray
Write-Host "  Each account has its own login; the data (projects/sessions/…) is shared." -ForegroundColor DarkGray
