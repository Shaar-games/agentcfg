$ErrorActionPreference = 'Stop'
$repo = 'https://github.com/Shaar-games/agentmux.git'
$dest = if ($env:AGENTMUX_INSTALL_DIR) { $env:AGENTMUX_INSTALL_DIR } else { Join-Path $HOME '.local\share\agentmux' }
if (Test-Path -LiteralPath $dest) { throw "Destination already exists: $dest (refusing to overwrite)" }
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
Write-Host "Installed: $(Join-Path $dest 'bin\agentmux.exe')"
Write-Host "Add $(Join-Path $dest 'bin') to PATH or use its absolute path."
