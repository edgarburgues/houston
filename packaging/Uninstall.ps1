#requires -Version 7.0
<#
.SYNOPSIS
  Desinstala Houston revirtiendo SOLO lo que puso el instalador: el binario, los
  bloques del perfil (PATH + alias `claude`) y la entrada de PATH de usuario en
  Windows.

.DESCRIPTION
  Seguro por defecto: NUNCA borra tus conversaciones. Los datos compartidos
  (~/.claude-shared) contienen las transcripciones reales y se conservan, igual
  que los logins por cuenta (~/.claude-accounts) y el store de Houston.

  -PurgeData borra además SOLO el store propio de Houston (~/.claude/houston, que
  guarda accounts.json) y la config de escaneo (~/.config/claudeswap). Sigue sin
  tocar shared/ ni los dirs de cuentas (evita seguir junctions). El resto se borra
  a mano siguiendo las instrucciones que imprime.

  Idempotente: seguro re-ejecutar.
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

Info "Houston — desinstalando"

# --- 1. binario -----------------------------------------------------------
$binDst = Join-Path $BinDir $exe
if (Test-Path $binDst) { Remove-Item $binDst -Force; Ok "binario eliminado ($binDst)" }
else { Note "binario no encontrado ($binDst)" }

# --- 2. bloques del perfil (PATH + alias claude) --------------------------
$prof = $PROFILE.CurrentUserAllHosts
if (Test-Path $prof) {
  $text = Get-Content $prof -Raw
  $orig = $text
  $text = [regex]::Replace($text, '(?ms)\r?\n?# >>> houston >>>.*?# <<< houston <<<\r?\n?', "`n")
  $text = [regex]::Replace($text, '(?ms)\r?\n?# >>> houston-claude >>>.*?# <<< houston-claude <<<\r?\n?', "`n")
  if ($text -ne $orig) {
    Set-Content -Path $prof -Value $text.TrimEnd() -Encoding utf8
    Ok "bloques del perfil eliminados ($prof)"
  } else { Note "el perfil no tenía bloques de houston" }
} else { Note "sin perfil ($prof)" }

# --- 3. PATH de usuario en Windows ----------------------------------------
if ($IsWindows) {
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if ($userPath) {
    $parts = $userPath -split ';' | Where-Object { $_ -and $_ -ne $BinDir }
    $new = $parts -join ';'
    if ($new -ne $userPath.TrimEnd(';')) {
      [Environment]::SetEnvironmentVariable('Path', $new, 'User')
      Ok "PATH de usuario (Windows) limpiado"
    } else { Note "PATH de usuario no contenía $BinDir" }
  }
}

# --- 4. datos (opcional) --------------------------------------------------
$store    = Join-Path $HOME '.claude/houston'
$swapCfg  = Join-Path $HOME '.config/claudeswap'
$shared   = Join-Path $HOME '.claude-shared'
$accounts = Join-Path $HOME '.claude-accounts'

if ($PurgeData) {
  foreach ($p in @($store, $swapCfg)) {
    if (Test-Path $p) { Remove-Item $p -Recurse -Force; Ok "borrado $p" }
  }
  Warn "se conservan tus conversaciones y logins:"
  Note "$shared     (transcripciones reales)"
  Note "$accounts   (login por cuenta)"
} else {
  Note "datos conservados (usa -PurgeData para borrar el store de Houston)"
}

Write-Host ""
Info "Listo. Houston desinstalado. Abre una terminal nueva para refrescar el PATH."
Write-Host "  Tus conversaciones siguen en: $shared" -ForegroundColor DarkGray
Write-Host "  Para borrarlas TODO a mano (irreversible):" -ForegroundColor DarkGray
Write-Host "    Remove-Item '$shared','$accounts','$store','$swapCfg' -Recurse -Force" -ForegroundColor DarkGray
