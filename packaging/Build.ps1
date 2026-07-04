#requires -Version 7.0
<#
.SYNOPSIS
  Maintenance build: cross-compile Houston for all platforms into packaging/bin,
  then assemble a distributable zip (houston-<ver>.zip).
#>
[CmdletBinding()]
param([string]$Version = 'dev')

$ErrorActionPreference = 'Stop'
$pkg  = $PSScriptRoot
$repo = Split-Path -Parent $pkg

$go = (Get-Command go -ErrorAction SilentlyContinue)?.Source
if (-not $go) {
  $cand = Join-Path $HOME 'go-sdk/go/bin/go.exe'
  if (Test-Path $cand) { $go = $cand }
}
if (-not $go) { throw "go not found; install Go ≥ 1.26" }

$targets = @(
  @{os='windows';arch='amd64';exe='houston.exe'},
  @{os='windows';arch='arm64';exe='houston.exe'},
  @{os='darwin'; arch='amd64';exe='houston'},
  @{os='darwin'; arch='arm64';exe='houston'},
  @{os='linux';  arch='amd64';exe='houston'},
  @{os='linux';  arch='arm64';exe='houston'}
)

Write-Host "Building ($Version)..." -ForegroundColor Cyan
Push-Location $repo
try {
  $env:CGO_ENABLED = '0'
  foreach ($t in $targets) {
    $env:GOOS = $t.os; $env:GOARCH = $t.arch
    & $go build -trimpath -ldflags "-s -w -X main.version=$Version" -o (Join-Path $pkg "bin/$($t.os)-$($t.arch)/$($t.exe)") .
    if ($LASTEXITCODE -ne 0) { throw "go build failed: $($t.os)-$($t.arch)" }
    Write-Host ("  ✓ {0}-{1}" -f $t.os, $t.arch) -ForegroundColor Green
  }
} finally {
  Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED -ErrorAction SilentlyContinue
  Pop-Location
}

# stage: houston/{Install.ps1, bin, README.md} — Install.ps1 finds bin/ beside it
$stageRoot = Join-Path $repo 'dist'
$stage = Join-Path $stageRoot 'houston'
if (Test-Path $stageRoot) { Remove-Item $stageRoot -Recurse -Force }
New-Item -ItemType Directory -Path $stage -Force | Out-Null
Copy-Item (Join-Path $pkg 'Install.ps1')                  $stage
Copy-Item (Join-Path $repo 'houston-setup-accounts.ps1')  $stage
Copy-Item (Join-Path $pkg 'bin')                          (Join-Path $stage 'bin') -Recurse
Copy-Item (Join-Path $repo 'README.md')                   $stage

$zip = Join-Path $stageRoot "houston-$Version.zip"
Compress-Archive -Path $stage -DestinationPath $zip -Force
Write-Host "`nzip: $zip ($([math]::Round((Get-Item $zip).Length/1MB,1)) MB)" -ForegroundColor Cyan
