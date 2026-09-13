#requires -Version 7.0
# Exercise installers using fake downloads and a failing compiler, never an install.
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$scratch = Join-Path ([IO.Path]::GetTempPath()) ('houston-script-tests-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $scratch | Out-Null
$originalStore = $env:HOUSTON_HOME
$originalPath = $env:PATH
$originalProfile = $env:USERPROFILE
$originalExit = $global:LASTEXITCODE
$global:HoustonInstallerFixture = @{ urls = [Collections.Generic.List[string]]::new(); failDownload = $false; cargoCalled = $false; hash = ''; badSignature = $false; wrongTag = $false; badChecksum = $false }
$fixture = Join-Path $scratch 'fixture'
[IO.File]::WriteAllText($fixture, 'fixture binary, never executed')
$global:HoustonInstallerFixture.hash = (Get-FileHash -LiteralPath $fixture -Algorithm SHA256).Hash

function Invoke-WebRequest {
    param($Uri, $OutFile, [switch]$UseBasicParsing)
    $global:HoustonInstallerFixture.urls.Add($Uri)
    if ($global:HoustonInstallerFixture.failDownload) { throw 'simulated download failure' }
    if ($Uri.EndsWith('/checksums.txt.minisig')) {
        $tag = if ($global:HoustonInstallerFixture.wrongTag) { 'v2.0.0' } else { 'v2.0.1' }
        [IO.File]::WriteAllText($OutFile, "untrusted comment: fixture`nfixture`ntrusted comment: tag:$tag`nfixture`n")
    } elseif ($Uri.EndsWith('/checksums.txt')) {
        $asset = ($global:HoustonInstallerFixture.urls[-2] -split '/')[-1]
        $hash = if ($global:HoustonInstallerFixture.badChecksum) { '0' * 64 } else { $global:HoustonInstallerFixture.hash }
        [IO.File]::WriteAllText($OutFile, "$hash  $asset")
    } else {
        [IO.File]::WriteAllText($OutFile, 'fixture binary, never executed')
    }
}
function Invoke-RestMethod { param($Uri); @{ tag_name = 'v2.0.1' } }
function minisign { $global:LASTEXITCODE = if ($global:HoustonInstallerFixture.badSignature) { 1 } else { 0 } }
function cargo {
    $global:HoustonInstallerFixture.cargoCalled = $true
    if ($global:HoustonInstallerFixture.buildRoot) {
        $dest = Join-Path $global:HoustonInstallerFixture.buildRoot 'target/x86_64-pc-windows-msvc/release'
        New-Item -ItemType Directory -Path $dest -Force | Out-Null
        [IO.File]::WriteAllText((Join-Path $dest 'houston.exe'), 'fake Rust binary')
        $global:LASTEXITCODE = 0
    } else { $global:LASTEXITCODE = 1 }
}

try {
    $env:HOUSTON_HOME = Join-Path $scratch 'store'
    $env:USERPROFILE = $scratch
    & (Join-Path $PSScriptRoot 'Install.ps1') -BinDir (Join-Path $scratch 'bin') -NoProfileEdit -Repo 'fixture/project'
    if ($global:HoustonInstallerFixture.urls.Count -ne 3 -or $global:HoustonInstallerFixture.urls[0] -notlike 'https://github.com/fixture/project/releases/download/v2.0.1/*') {
        throw "Installer lost the repository argument: $($global:HoustonInstallerFixture.urls)"
    }
    if ($global:HoustonInstallerFixture.cargoCalled) { throw 'A valid download must not invoke Cargo' }

    foreach ($failure in @('badSignature', 'wrongTag', 'badChecksum')) {
        $global:HoustonInstallerFixture[$failure] = $true
        $failed = $false
        $dest = Join-Path $scratch $failure
        try { & (Join-Path $PSScriptRoot 'Install.ps1') -BinDir $dest -NoProfileEdit }
        catch { $failed = $true }
        if (-not $failed -or (Get-ChildItem -LiteralPath $dest -File) -or $global:HoustonInstallerFixture.cargoCalled) {
            throw "Unauthenticated release was accepted: $failure"
        }
        $global:HoustonInstallerFixture[$failure] = $false
    }

    $global:HoustonInstallerFixture.failDownload = $true
    $failed = $false
    try {
        & (Join-Path $PSScriptRoot 'Install.ps1') -BinDir (Join-Path $scratch 'failed-bin') -NoProfileEdit
    } catch {
        $failed = $_.Exception.Message -match 'cargo build failed'
    }
    if (-not $failed -or -not $global:HoustonInstallerFixture.cargoCalled) { throw 'A failed Cargo fallback must fail the installer' }

    $failed = $false
    try { & (Join-Path $root 'install-local.ps1') }
    catch { $failed = $_.Exception.Message -match 'installation cancelled' }
    if (-not $failed) { throw 'Local installation must stop after a failed build' }
    if (Test-Path (Join-Path $scratch '.local/bin/houston.exe')) { throw 'Failed build installed a binary' }
    # Package only a synthetic repository and binary, keeping real build output intact.
    $buildRoot = Join-Path $scratch 'build-fixture'
    New-Item -ItemType Directory -Path (Join-Path $buildRoot 'packaging') -Force | Out-Null
    foreach ($name in @('Build.ps1', 'Install.ps1')) {
        Copy-Item -LiteralPath (Join-Path $PSScriptRoot $name) -Destination (Join-Path $buildRoot 'packaging')
    }
    [IO.File]::WriteAllText((Join-Path $buildRoot 'Cargo.toml'), "[workspace.package]`nversion = `"2.0.1`"`n")
    foreach ($name in @('README.md', 'LICENSE')) { [IO.File]::WriteAllText((Join-Path $buildRoot $name), 'fixture') }
    $global:HoustonInstallerFixture.buildRoot = $buildRoot
    & (Join-Path $buildRoot 'packaging/Build.ps1') -Target 'x86_64-pc-windows-msvc'
    $archive = [IO.Compression.ZipFile]::OpenRead((Join-Path $buildRoot 'dist/houston-2.0.1-windows-amd64.zip'))
    try {
        $names = @($archive.Entries | ForEach-Object { $_.FullName.Replace('\', '/') })
        if ($names -notcontains 'houston/bin/windows-amd64/houston.exe') { throw 'Archive has no Rust binary' }
        if ($names -notcontains 'houston/Install.ps1') { throw 'Archive has no installer' }
    } finally { $archive.Dispose() }
    Write-Host 'Installer regressions passed (fake downloads, no real installation).'
} finally {
    $env:HOUSTON_HOME = $originalStore
    $env:PATH = $originalPath
    $env:USERPROFILE = $originalProfile
    $global:LASTEXITCODE = $originalExit
    Remove-Variable -Name HoustonInstallerFixture -Scope Global
}
