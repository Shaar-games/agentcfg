# Windows PowerShell 5.1+/PowerShell 7 installer: precompiled releases only.
$ErrorActionPreference = 'Stop'
$repo = 'https://github.com/Shaar-games/agentmux'
$version = if ($env:AGENTMUX_VERSION) { $env:AGENTMUX_VERSION } else { 'latest' }
if ($version -eq 'latest') {
    $base = "$repo/releases/latest/download"
} elseif ($version -match '^v[0-9]+\.[0-9]+\.[0-9]+([-+._a-zA-Z0-9]*)?$') {
    $base = "$repo/releases/download/$version"
} else {
    throw "Invalid AGENTMUX_VERSION: $version"
}
# Override only for testing against an independently hosted release mirror.
if ($env:AGENTMUX_RELEASE_BASE_URL) { $base = $env:AGENTMUX_RELEASE_BASE_URL }
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
$asset = "agentmux_windows_$arch.zip"
$bindir = if ($env:AGENTMUX_BIN_DIR) { $env:AGENTMUX_BIN_DIR } else { Join-Path $HOME '.local\bin' }
$binary = Join-Path $bindir 'agentmux.exe'
if (Test-Path -LiteralPath $binary) { throw "Refusing to overwrite existing executable: $binary" }

$tmp = Join-Path ([IO.Path]::GetTempPath()) ('agentmux-install-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
$stage = $null
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
    $actual = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -cne $expected) { throw "SHA-256 mismatch for $asset. Installation cancelled." }
    $unpacked = Join-Path $tmp 'unpacked'
    Expand-Archive -LiteralPath $archive -DestinationPath $unpacked
    $executable = Join-Path $unpacked 'agentmux.exe'
    if (-not (Test-Path -LiteralPath $executable -PathType Leaf)) { throw 'Archive has no agentmux.exe' }
    New-Item -ItemType Directory -Force -Path $bindir | Out-Null
    $stage = Join-Path $bindir ('.agentmux-install-' + [guid]::NewGuid().ToString('N') + '.exe')
    Copy-Item -LiteralPath $executable -Destination $stage
    if (Test-Path -LiteralPath $binary) { throw "Destination appeared during installation: $binary" }
    # File.Move refuses an existing destination, including if another installer
    # created it after the check above. Move-Item is not an exclusive install.
    [System.IO.File]::Move($stage, $binary)
    $stage = $null
    & $binary version | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Remove-Item -LiteralPath $binary -Force -ErrorAction SilentlyContinue
        throw 'Binary smoke test failed; installation rolled back.'
    }
    Write-Host "Installed $binary"
    Write-Host "Add $bindir to PATH if necessary."
} finally {
    if ($stage -and (Test-Path -LiteralPath $stage)) { Remove-Item -LiteralPath $stage -Force -ErrorAction SilentlyContinue }
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
