#requires -Version 7.0
<#
.SYNOPSIS
  Uninstalls Houston, reverting ONLY what the installer put in place: the binary,
  the profile blocks (PATH + `claude` alias) and the user PATH entry on Windows.

.DESCRIPTION
  Safe by default: it NEVER deletes your conversations. The shared data
  (~/.claude-shared) holds the real transcripts and is kept, as are the
  per-account logins (~/.claude-accounts) and Houston's store.

  -PurgeData additionally deletes ONLY Houston's own store (~/.claude/houston,
  which holds accounts.json) and the scan config (~/.config/claudeswap). It
  still doesn't touch shared/ or the account dirs (avoids following junctions).
  Everything else is deleted by hand following the instructions it prints.

  Idempotent: safe to re-run.
#>
[CmdletBinding()]
param(
  [string]$BinDir,
  [switch]$PurgeData
)

$ErrorActionPreference = 'Stop'
function Info($m){ Write-Host $m -ForegroundColor Cyan }
function Ok($m){ Write-Host "  ✓ $m" -ForegroundColor Green }
function Warn($m){ Write-Host "  ! $m" -ForegroundColor Yellow }
function Note($m){ Write-Host "  . $m" -ForegroundColor DarkGray }

if (-not $BinDir) { $BinDir = Join-Path $HOME '.local/bin' }
$exe = if ($IsWindows) { 'houston.exe' } else { 'houston' }

Info "Houston — uninstalling"

# --- 1. binary --------------------------------------------------------------
$binDst = Join-Path $BinDir $exe
if (Test-Path $binDst) { Remove-Item $binDst -Force; Ok "binary removed ($binDst)" }
else { Note "binary not found ($binDst)" }

# --- 2. profile blocks (PATH + claude alias) --------------------------------
$prof = $PROFILE.CurrentUserAllHosts
if (Test-Path $prof) {
  $text = Get-Content $prof -Raw
  $orig = $text
  $text = [regex]::Replace($text, '(?ms)\r?\n?# >>> houston >>>.*?# <<< houston <<<\r?\n?', "`n")
  $text = [regex]::Replace($text, '(?ms)\r?\n?# >>> houston-claude >>>.*?# <<< houston-claude <<<\r?\n?', "`n")
  if ($text -ne $orig) {
    Set-Content -Path $prof -Value $text.TrimEnd() -Encoding utf8
    Ok "profile blocks removed ($prof)"
  } else { Note "the profile had no houston blocks" }
} else { Note "no profile ($prof)" }

# --- 3. user PATH on Windows -------------------------------------------------
if ($IsWindows) {
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if ($userPath) {
    $parts = $userPath -split ';' | Where-Object { $_ -and $_ -ne $BinDir }
    $new = $parts -join ';'
    if ($new -ne $userPath.TrimEnd(';')) {
      [Environment]::SetEnvironmentVariable('Path', $new, 'User')
      Ok "user PATH (Windows) cleaned"
    } else { Note "user PATH did not contain $BinDir" }
  }
}

# --- 4. data (optional) -------------------------------------------------------
$store    = Join-Path $HOME '.claude/houston'
$swapCfg  = Join-Path $HOME '.config/claudeswap'
$shared   = Join-Path $HOME '.claude-shared'
$accounts = Join-Path $HOME '.claude-accounts'

if ($PurgeData) {
  foreach ($p in @($store, $swapCfg)) {
    if (Test-Path $p) { Remove-Item $p -Recurse -Force; Ok "deleted $p" }
  }
  Warn "your conversations and logins are kept:"
  Note "$shared     (real transcripts)"
  Note "$accounts   (per-account logins)"
} else {
  Note "data kept (use -PurgeData to delete Houston's store)"
}

Write-Host ""
Info "Done. Houston uninstalled. Open a new terminal to refresh the PATH."
Write-Host "  Your conversations are still in: $shared" -ForegroundColor DarkGray
Write-Host "  To delete EVERYTHING by hand (irreversible):" -ForegroundColor DarkGray
Write-Host "    Remove-Item '$shared','$accounts','$store','$swapCfg' -Recurse -Force" -ForegroundColor DarkGray
