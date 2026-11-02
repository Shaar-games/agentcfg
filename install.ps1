$ErrorActionPreference = 'Stop'
$repo = 'https://github.com/Shaar-games/agentmux.git'
$dest = if ($env:AGENTMUX_INSTALL_DIR) { $env:AGENTMUX_INSTALL_DIR } else { Join-Path $HOME '.local\share\agentmux' }
$binDir = Join-Path $HOME '.local\bin'
$binary = Join-Path $binDir 'agentmux.exe'
if (Test-Path -LiteralPath $dest) { throw "Destination already exists: $dest (refusing to overwrite)" }
if (Test-Path -LiteralPath $binary) { throw "Executable already exists: $binary (refusing to overwrite)" }
foreach ($tool in @('git', 'mise')) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "$tool is required" }
}
$parent = Split-Path -Parent $dest
New-Item -ItemType Directory -Force -Path $parent | Out-Null
& git clone --depth 1 $repo $dest
if ($LASTEXITCODE -ne 0) { throw 'git clone failed' }
Push-Location $dest
try {
    & mise trust
    if ($LASTEXITCODE -ne 0) { throw 'mise trust failed' }
    & mise install
    if ($LASTEXITCODE -ne 0) { throw 'mise install failed' }
    & mise run build
    if ($LASTEXITCODE -ne 0) { throw 'mise build failed' }
} finally { Pop-Location }
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
Copy-Item -LiteralPath (Join-Path $dest 'bin\agentmux.exe') -Destination $binary -ErrorAction Stop
Write-Host "Installed: $binary"
Write-Host "Add $binDir to PATH if needed."
