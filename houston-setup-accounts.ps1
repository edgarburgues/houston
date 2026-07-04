#requires -Version 7.0
<#
.SYNOPSIS
  Houston multi-account layout: one CLAUDE_CONFIG_DIR per account (isolated login
  + onboarding) sharing conversations/config with the rest via Windows JUNCTIONS
  (symlinks on macOS/Linux). Idempotent: safe to re-run; never clobbers an
  account's existing login (.credentials.json) or its .claude.json once created.

  Layout:
    ~/.claude-shared/{projects,sessions,plugins,plans,todos,skills}  = real dirs (truth)
    ~/.claude-accounts/account-<id>/                          = per-account dir
        .claude.json / .credentials.json   -> per account (NOT linked)
        projects,sessions,plugins,plans,todos,skills -> JUNCTION -> ~/.claude-shared/*
        settings.json, mcp.json             -> seeded identical
#>
[CmdletBinding()]
param([switch]$ResyncSettings)   # -ResyncSettings re-pushes shared settings.json/mcp.json into every account

$ErrorActionPreference = 'Stop'
function Info($m){ Write-Host $m -ForegroundColor Cyan }
function Ok($m){ Write-Host "  + $m" -ForegroundColor Green }
function Note($m){ Write-Host "  . $m" -ForegroundColor DarkGray }

$shared        = Join-Path $HOME '.claude-shared'
$accountsRoot  = Join-Path $HOME '.claude-accounts'
$accountsJson  = Join-Path $HOME '.claude\houston\accounts.json'
$srcConfig     = Join-Path $HOME '.claude.json'            # onboarded template (hasCompletedOnboarding=true)
$srcSettings   = Join-Path $HOME '.claude\settings.json'
$srcMcp        = Join-Path $HOME '.claude\mcp.json'
# Keep in sync with provision.ShareDirs in the Go code (houston doctor uses that
# list). Data dirs first, then user-level customizations shared across accounts.
$shareDirs     = @('projects','sessions','plugins','plans','todos',
                   'skills','commands','agents','workflows','rules','output-styles','themes')

$isWin = $IsWindows
function New-Link($link, $target) {
  if ($isWin) { New-Item -ItemType Junction -Path $link -Target $target | Out-Null }
  else        { New-Item -ItemType SymbolicLink -Path $link -Target $target | Out-Null }
}

# --- 1. shared store: ensure the shared dirs exist as real dirs --------------
Info "1) Shared store: $shared"
New-Item -ItemType Directory -Force -Path $shared | Out-Null
foreach ($d in $shareDirs) {
  $p = Join-Path $shared $d
  if (-not (Test-Path $p)) { New-Item -ItemType Directory -Force -Path $p | Out-Null; Ok "shared/$d" } else { Note "shared/$d already exists" }
}
# seed shared settings/mcp from the current ~/.claude if shared copy missing
if ((Test-Path $srcSettings) -and -not (Test-Path (Join-Path $shared 'settings.json'))) { Copy-Item $srcSettings (Join-Path $shared 'settings.json'); Ok 'shared/settings.json' }
if ((Test-Path $srcMcp)      -and -not (Test-Path (Join-Path $shared 'mcp.json')))      { Copy-Item $srcMcp      (Join-Path $shared 'mcp.json');      Ok 'shared/mcp.json' }
# seed shared/skills from ~/.claude/skills if the shared copy is still empty (don't
# lose user-level skills you already had before sharing was enabled).
$srcSkills = Join-Path $HOME '.claude\skills'; $dstSkills = Join-Path $shared 'skills'
if ((Test-Path $srcSkills) -and -not (Get-ChildItem $dstSkills -Force -ErrorAction SilentlyContinue)) {
  Copy-Item (Join-Path $srcSkills '*') $dstSkills -Recurse -Force -ErrorAction SilentlyContinue
  Ok 'shared/skills (seeded from ~/.claude/skills)'
}

# --- 2. seed template: ~/.claude.json minus oauthAccount (fresh identity) ----
# Native PowerShell JSON (no python dependency). If there's no source config yet
# (virgin machine), seed a minimal onboarded template.
$seed = Join-Path ([IO.Path]::GetTempPath()) 'houston-seed-claude.json'
if (Test-Path $srcConfig) {
  $cfg = Get-Content $srcConfig -Raw | ConvertFrom-Json
  $cfg.PSObject.Properties.Remove('oauthAccount')   # drop identity: each account logs in on its own
} else {
  $cfg = [pscustomobject]@{ hasCompletedOnboarding = $true }
}
$cfg | ConvertTo-Json -Depth 100 | Set-Content $seed -Encoding utf8

# --- 3. per-account dirs + junctions + seed ----------------------------------
Info "2) Per-account dirs: $accountsRoot"
New-Item -ItemType Directory -Force -Path $accountsRoot | Out-Null
if (-not (Test-Path $accountsJson)) { throw "missing $accountsJson — create accounts first: houston account add <label>" }
$accs = @(Get-Content $accountsJson -Raw | ConvertFrom-Json)
if ($accs.Count -eq 0) { throw "no accounts in $accountsJson — add one: houston account add <label>" }
foreach ($a in $accs) {
  $dir = Join-Path $accountsRoot ("account-" + $a.id)
  New-Item -ItemType Directory -Force -Path $dir | Out-Null

  $cj = Join-Path $dir '.claude.json'
  if (-not (Test-Path $cj)) { Copy-Item $seed $cj; Ok "account-$($a.id)/.claude.json (seed)" } else { Note "account-$($a.id)/.claude.json kept" }

  foreach ($f in @('settings.json','mcp.json')) {
    $src = Join-Path $shared $f; $dst = Join-Path $dir $f
    if ((Test-Path $src) -and ($ResyncSettings -or -not (Test-Path $dst))) { Copy-Item $src $dst -Force }
  }

  foreach ($d in $shareDirs) {
    $link = Join-Path $dir $d; $target = Join-Path $shared $d
    if (Test-Path $link) {
      $it = Get-Item $link -Force
      if ($it.LinkType) { Note "account-$($a.id)/$d already linked"; continue }
      # real dir (not a link): migrate its content into shared, then replace with
      # a link — never delete data.
      $kids = @(Get-ChildItem $link -Force -ErrorAction SilentlyContinue)
      if ($kids.Count -gt 0) {
        if ($isWin) {
          & robocopy $link $target /E /XC /XN /XO /NFL /NDL /NP /NJH /NJS *> $null
          if ($LASTEXITCODE -ge 8) { throw "robocopy failed (exit $LASTEXITCODE) merging $link -> $target; the original is NOT renamed (check $link)" }
        } else {
          Get-ChildItem $link -Recurse -File | ForEach-Object {
            $rel = $_.FullName.Substring($link.Length).TrimStart([char]'/', [char]'\')
            $to  = Join-Path $target $rel
            $td  = Split-Path $to -Parent
            if (-not (Test-Path $td)) { New-Item -ItemType Directory -Force -Path $td | Out-Null }
            if (-not (Test-Path $to)) { Copy-Item $_.FullName $to }
          }
        }
        $orphan = "$link.orphaned-{0}" -f (Get-Date -Format 'yyyyMMdd-HHmmss')
        Rename-Item $link $orphan
        Note "account-$($a.id)/$d had real content -> merged into shared (original: $(Split-Path $orphan -Leaf))"
      } else {
        Remove-Item $link -Force   # empty dir: safe to drop
      }
    }
    New-Link $link $target
    Ok "account-$($a.id)/$d -> junction"
  }
}

# --- 4. claudeswap config so Houston's TUI scans this layout -----------------
Info "3) Config for the TUI scan"
$cfgDir = Join-Path $HOME '.config\claudeswap'
New-Item -ItemType Directory -Force -Path $cfgDir | Out-Null
[pscustomobject]@{ accountsDir = $accountsRoot; sharedDir = $shared } |
  ConvertTo-Json | Set-Content (Join-Path $cfgDir 'config.json') -Encoding utf8
Ok "$cfgDir\config.json"

Write-Host ""
Info "Done. Accounts configured:"
foreach ($a in $accs) { Write-Host "  $($a.id)  ->  $(Join-Path $accountsRoot ('account-'+$a.id))" }
Write-Host ""
Write-Host "First launch of each account: 'houston run' (if it asks to log in, do it once; it is stored per account)." -ForegroundColor Yellow
