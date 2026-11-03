# Windows PowerShell 5.1+/PowerShell 7 installer: precompiled releases only.
$ErrorActionPreference = 'Stop'
$repo = 'https://github.com/Shaar-games/agentcfg'
$version = if ($env:AGENTCFG_VERSION) { $env:AGENTCFG_VERSION } else { 'latest' }
if ($version -eq 'latest') {
    $base = "$repo/releases/latest/download"
} elseif ($version -match '^v[0-9]+\.[0-9]+\.[0-9]+([-+._a-zA-Z0-9]*)?$') {
    $base = "$repo/releases/download/$version"
} else {
    throw "Invalid AGENTCFG_VERSION: $version"
}
# Override only for testing against an independently hosted release mirror.
if ($env:AGENTCFG_RELEASE_BASE_URL) { $base = $env:AGENTCFG_RELEASE_BASE_URL }
$base = $base.TrimEnd('/')

try {
    $architecture = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
} catch {
    $architecture = $env:PROCESSOR_ARCHITECTURE.ToLowerInvariant()
}
switch ($architecture) {
    { $_ -in @('x64', 'amd64') } { $arch = 'amd64'; break }
    { $_ -in @('arm64', 'aarch64') } { $arch = 'arm64'; break }
    default { throw "Unsupported Windows architecture: $architecture" }
}
$asset = "agentcfg_windows_$arch.zip"
$bindir = if ($env:AGENTCFG_BIN_DIR) { $env:AGENTCFG_BIN_DIR } else { Join-Path $HOME '.local\bin' }
$binary = Join-Path $bindir 'agentcfg.exe'

$tmp = Join-Path ([IO.Path]::GetTempPath()) ('agentcfg-install-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
$stage = $null
$backup = $null
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    $archive = Join-Path $tmp $asset
    $manifest = Join-Path $tmp 'SHA256SUMS'
    Write-Host "Downloading precompiled $asset ($version)..."
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $archive
    Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $manifest
    $expected = $null
    foreach ($line in (Get-Content -LiteralPath $manifest)) {
        if ($line -match '^([a-fA-F0-9]{64})\s+\*?(.+)$' -and $Matches[2] -ceq $asset) {
            if ($null -ne $expected) { throw "Duplicate checksum entry for $asset" }
            $expected = $Matches[1].ToLowerInvariant()
        }
    }
    if ($null -eq $expected) { throw "Missing SHA-256 checksum entry for $asset" }
    # Use the .NET SHA-256 implementation: Get-FileHash is not available on
    # every Windows PowerShell runner, even when Invoke-WebRequest works.
    $stream = [IO.File]::OpenRead($archive)
    try {
        $sha = [Security.Cryptography.SHA256]::Create()
        try { $digest = $sha.ComputeHash($stream) } finally { $sha.Dispose() }
    } finally { $stream.Dispose() }
    $actual = ([BitConverter]::ToString($digest)).Replace('-', '').ToLowerInvariant()
    if ($actual -cne $expected) { throw "SHA-256 mismatch for $asset. Installation cancelled." }
    $unpacked = Join-Path $tmp 'unpacked'
    New-Item -ItemType Directory -Path $unpacked | Out-Null
    $executable = Join-Path $unpacked 'agentcfg.exe'
    # Extract only the expected regular file; do not unpack other zip entries.
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [IO.Compression.ZipFile]::OpenRead($archive)
    try {
        $entries = @($zip.Entries | Where-Object { $_.FullName -ceq 'agentcfg.exe' })
        if ($entries.Count -ne 1 -or $entries[0].Length -le 0 -or $entries[0].Length -gt 268435456) {
            throw 'Archive must contain exactly one nonempty agentcfg.exe of reasonable size'
        }
        $inputStream = $entries[0].Open()
        try {
            $outputStream = [IO.File]::Create($executable)
            try { $inputStream.CopyTo($outputStream) } finally { $outputStream.Dispose() }
        } finally { $inputStream.Dispose() }
    } finally { $zip.Dispose() }
    New-Item -ItemType Directory -Force -Path $bindir | Out-Null
    $stage = Join-Path $bindir ('.agentcfg-install-' + [guid]::NewGuid().ToString('N') + '.exe')
    Copy-Item -LiteralPath $executable -Destination $stage
    & $stage version | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Staged binary failed; previous installation kept.' }
    $placed = $false
    try {
        if (Test-Path -LiteralPath $binary) {
            $existing = Get-Item -LiteralPath $binary -Force
            if ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
                throw "Existing destination must be a regular executable: $binary"
            }
            $backup = Join-Path $bindir ('.agentcfg-backup-' + [guid]::NewGuid().ToString('N') + '.exe')
            # Windows can rename a running executable but cannot overwrite it
            # in place. Keep the previous image for rollback.
            [System.IO.File]::Move($binary, $backup)
        }
        [System.IO.File]::Move($stage, $binary)
        $stage = $null
        $placed = $true
        & $binary version | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Installed binary smoke test failed.' }
    } catch {
        if ($placed -and (Test-Path -LiteralPath $binary)) {
            Remove-Item -LiteralPath $binary -Force
        }
        if ($backup -and (Test-Path -LiteralPath $backup)) {
            [System.IO.File]::Move($backup, $binary)
            $backup = $null
        }
        throw
    }
    if ($backup -and (Test-Path -LiteralPath $backup)) {
        try { Remove-Item -LiteralPath $backup -Force; $backup = $null } catch {
            Write-Warning "Previous executable still in use; backup retained at $backup"
        }
    }
    Write-Host "Installed/updated $binary"
    Write-Host "Add $bindir to PATH if necessary."
} finally {
    if ($stage -and (Test-Path -LiteralPath $stage)) { Remove-Item -LiteralPath $stage -Force -ErrorAction SilentlyContinue }
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
