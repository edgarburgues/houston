#requires -Version 7.0
# Build one Rust target and package a local installable archive.
[CmdletBinding()]
param([string]$Version, [string]$Target)
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$manifest = Get-Content -LiteralPath (Join-Path $repoRoot 'Cargo.toml') -Raw
$sourceVersion = [regex]::Match($manifest, '(?m)^version\s*=\s*"([^"\r\n]+)"').Groups[1].Value
if (-not $sourceVersion) { throw 'No workspace version in Cargo.toml' }
if ($Version -and $Version.TrimStart('v') -ne $sourceVersion) { throw "Version must match Cargo.toml ($sourceVersion)" }
$Version = $sourceVersion
if (-not $Target) {
    $rustInfo = & rustc -vV
    if ($LASTEXITCODE -ne 0) { throw 'rustc failed' }
    $Target = ($rustInfo | Select-String '^host: ').Line.Substring(6).Trim()
}
$platforms = @{
    'x86_64-pc-windows-msvc' = 'windows-amd64'
    'x86_64-unknown-linux-gnu' = 'linux-amd64'
    'aarch64-unknown-linux-gnu' = 'linux-arm64'
    'x86_64-apple-darwin' = 'darwin-amd64'
    'aarch64-apple-darwin' = 'darwin-arm64'
}
$platform = $platforms[$Target]
if (-not $platform) { throw "Unsupported release target: $Target" }
$exe = if ($platform.StartsWith('windows-')) { 'houston.exe' } else { 'houston' }
Push-Location $repoRoot
try {
    & cargo build --locked --release -p houston --target $Target
    if ($LASTEXITCODE -ne 0) { throw "cargo build failed for $Target" }
    $stage = Join-Path $repoRoot ('dist/stage-' + [guid]::NewGuid().ToString('N'))
    $binDir = Join-Path $stage "houston/bin/$platform"
    New-Item -ItemType Directory -Path $binDir -Force | Out-Null
    Copy-Item -LiteralPath (Join-Path $repoRoot "target/$Target/release/$exe") -Destination $binDir
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'Install.ps1') -Destination (Join-Path $stage 'houston')
    Copy-Item -LiteralPath (Join-Path $repoRoot 'README.md'),(Join-Path $repoRoot 'LICENSE') -Destination (Join-Path $stage 'houston')
    $zip = Join-Path $repoRoot "dist/houston-$Version-$platform.zip"
    Compress-Archive -LiteralPath (Join-Path $stage 'houston') -DestinationPath $zip -Force
    Write-Host "Built $zip"
} finally { Pop-Location }
