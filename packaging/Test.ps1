#requires -Version 7.0
# Run Cargo with disposable application paths, including fallback home paths.
[CmdletBinding()]
param([string[]]$CargoArgs = @('test', '--locked', '--workspace'))
$ErrorActionPreference = 'Stop'
$originalHome = if ($IsWindows) { $env:USERPROFILE } else { $env:HOME }
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('houston-tests-' + [guid]::NewGuid().ToString('N'))
$overrides = @{
    HOME = $testRoot
    USERPROFILE = $testRoot
    HOUSTON_HOME = (Join-Path $testRoot 'store')
    HOUSTON_SHARED_DIR = (Join-Path $testRoot 'shared')
    HOUSTON_ACCOUNTS_DIR = (Join-Path $testRoot 'accounts')
    HOUSTON_DEFAULT_SCOPE = '0'
    CLAUDE_CONFIG_DIR = (Join-Path $testRoot 'claude-config')
    CARGO_HOME = $(if ($env:CARGO_HOME) { $env:CARGO_HOME } else { Join-Path $originalHome '.cargo' })
    RUSTUP_HOME = $(if ($env:RUSTUP_HOME) { $env:RUSTUP_HOME } else { Join-Path $originalHome '.rustup' })
}
$previous = @{}
New-Item -ItemType Directory -Path $testRoot -Force | Out-Null
Push-Location (Split-Path -Parent $PSScriptRoot)
try {
    foreach ($name in $overrides.Keys) {
        $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        [Environment]::SetEnvironmentVariable($name, $overrides[$name], 'Process')
    }
    Write-Host "Isolated test home: $testRoot"
    & cargo @CargoArgs
    if ($LASTEXITCODE -ne 0) { throw "cargo failed ($LASTEXITCODE); fixtures kept at $testRoot" }
} finally {
    foreach ($name in $previous.Keys) {
        [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process')
    }
    Pop-Location
}
