#requires -Version 7.0
# Open a fresh disposable Houston home. No real accounts or credentials are copied.
[CmdletBinding()]
param([Parameter(ValueFromRemainingArguments)][string[]]$CommandArgs)
$ErrorActionPreference = 'Stop'
$binary = Join-Path $PSScriptRoot 'target/release/houston.exe'
if (-not $IsWindows) { $binary = Join-Path $PSScriptRoot 'target/release/houston' }
if (-not (Test-Path -LiteralPath $binary)) { throw 'Build first: cargo build --locked --release -p houston' }
$previewRoot = Join-Path ([IO.Path]::GetTempPath()) ('houston-preview-' + [guid]::NewGuid().ToString('N'))
$overrides = @{
    HOME = $previewRoot
    USERPROFILE = $previewRoot
    HOUSTON_HOME = (Join-Path $previewRoot 'store')
    HOUSTON_SHARED_DIR = (Join-Path $previewRoot 'shared')
    HOUSTON_ACCOUNTS_DIR = (Join-Path $previewRoot 'accounts')
    HOUSTON_DEFAULT_SCOPE = '0'
    CLAUDE_CONFIG_DIR = (Join-Path $previewRoot 'claude-config')
}
$previous = @{}
New-Item -ItemType Directory -Path $previewRoot | Out-Null
try {
    foreach ($name in $overrides.Keys) {
        $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        [Environment]::SetEnvironmentVariable($name, $overrides[$name], 'Process')
    }
    Write-Host "Disposable preview: $previewRoot (no real accounts or conversations)"
    & $binary @CommandArgs
    if ($LASTEXITCODE -ne 0) { throw "Houston exited with code $LASTEXITCODE" }
} finally {
    foreach ($name in $previous.Keys) {
        [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process')
    }
}
