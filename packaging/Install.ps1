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
  [string]$Version = 'latest',          # tag de release a descargar (o 'latest')
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

Info "Houston — instalando"
Write-Host "  bin:   $BinDir"
Write-Host "  store: $storeDir"

# --- 1. binary ------------------------------------------------------------
# Prioridad: (a) binario local junto al script (distribución por zip) ->
# (b) descarga desde GitHub Releases verificando SHA-256 -> (c) compilar con Go.
Info "1) Binario houston"
$exe   = if ($IsWindows) { 'houston.exe' } else { 'houston' }
$plat  = Get-Platform
$asset = if ($IsWindows) { "houston-$plat.exe" } else { "houston-$plat" }
New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
$binDst   = Join-Path $BinDir $exe
$prebuilt = Join-Path $pkg "bin/$plat/$exe"

if (Test-Path $prebuilt) {
  # (a) binario local (zip)
  Copy-Item $prebuilt $binDst -Force
  if (-not $IsWindows) { chmod +x $binDst }
  Ok "houston ($plat) -> $binDst (local)"
} else {
  # (b) descarga + verificación de checksum
  $base = if ($Version -eq 'latest') {
    "https://github.com/$Repo/releases/latest/download"
  } else {
    "https://github.com/$Repo/releases/download/$Version"
  }
  $downloaded = $false
  try {
    $tmp  = Join-Path ([IO.Path]::GetTempPath()) $asset
    $sums = Join-Path ([IO.Path]::GetTempPath()) 'houston-checksums.txt'
    Warn "descargando $asset desde Releases ($Version)..."
    Invoke-WebRequest -Uri "$base/$asset"        -OutFile $tmp  -UseBasicParsing
    Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile $sums -UseBasicParsing
    $line = Select-String -Path $sums -Pattern ([regex]::Escape($asset) + '\s*$') | Select-Object -First 1
    if (-not $line) { throw "no encuentro $asset en checksums.txt" }
    $expected = ($line.Line -split '\s+')[0]
    $actual   = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash
    if ($actual -ine $expected) {
      throw "checksum NO coincide para ${asset}: esperado $expected, obtenido $actual"
    }
    Copy-Item $tmp $binDst -Force
    if (-not $IsWindows) { chmod +x $binDst }
    Ok "houston ($plat) -> $binDst (release $Version, SHA-256 verificado)"
    $downloaded = $true
  } catch {
    Warn "descarga/verificación falló: $($_.Exception.Message)"
  }
  if (-not $downloaded) {
    # (c) compilar desde fuente
    if (Get-Command go -ErrorAction SilentlyContinue) {
      Warn "compilando con go..."
      Push-Location $repo
      try {
        & go build -o $binDst .
        if ($LASTEXITCODE -ne 0 -or -not (Test-Path $binDst)) { throw "go build falló" }
        Ok "compilado -> $binDst"
      } finally { Pop-Location }
    } else {
      throw "no pude descargar el binario para $plat ni hay Go para compilar; descarga el zip de Releases o instala Go"
    }
  }
}

# --- 2. data dir + setup script -------------------------------------------
Info "2) Carpeta de datos y script de setup"
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
    Ok "perfil actualizado ($prof)"
  } else { Ok "perfil ya configurado" }

  # alias `claude` -> `houston run`: que `claude ...` se sienta normal pero lo
  # orqueste Houston (elige cuenta, fija CLAUDE_CONFIG_DIR y lanza el claude real).
  # Es una función de shell, así que no choca con el claude.exe del PATH (Houston
  # lo resuelve por ruta absoluta en su proceso hijo).
  $profText = if (Test-Path $prof) { Get-Content $prof -Raw } else { '' }
  $cmarker = '# >>> houston-claude >>>'
  if ($profText -notmatch [regex]::Escape($cmarker)) {
    $cblock = @'
# >>> houston-claude >>>
# `claude ...` se enruta por Houston. Para buscar/retomar conversaciones: houston
function claude { houston run @args }
# <<< houston-claude <<<
'@
    Add-Content -Path $prof -Value "`n$cblock`n"
    Ok "alias claude -> houston run anadido"
  } else { Ok "alias claude ya configurado" }

  # Windows: also persist to the user environment so cmd/GUI sessions find it.
  if ($IsWindows) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($userPath -split ';') -notcontains $BinDir) {
      [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $BinDir), 'User')
      Ok "PATH de usuario (Windows) actualizado"
    }
  }
}

Write-Host ""
Info "Listo. Pasos siguientes (en una terminal nueva):"
Write-Host "  1) registra cada cuenta:   houston account add <etiqueta>"
Write-Host "  2) crea dirs + enlaces:    pwsh `"$storeDir\setup-accounts.ps1`""
Write-Host "  3) lanza (login 1ª vez):   houston run   (o simplemente: claude)"
Write-Host "  4) gestiona/retoma:        houston"
Write-Host ""
Write-Host "  'claude ...' queda enrutado por Houston (= 'houston run ...'). Para buscar/retomar: houston." -ForegroundColor DarkGray
Write-Host "  Cada cuenta tiene su propio login; los datos (projects/sessions/…) se comparten." -ForegroundColor DarkGray
