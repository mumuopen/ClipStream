# Build a ClipStream deploy package FOR THE TARGET MACHINE (run on dev machine).
# Output: .temp\clipstream-deploy.zip
# Contains: source + config.example + deploy scripts
# Excludes: data/ (DB & cookies), config.yaml (real keys), bin/, engines/

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$stage = Join-Path $root '.temp\deploy-stage'
$zip = Join-Path $root '.temp\clipstream-deploy.zip'

if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Path $stage -Force | Out-Null

$items = @(
  'cmd', 'internal', 'configs', 'deploy',
  'go.mod', 'go.sum',
  'config.example.yaml', 'LICENSE', 'README.md', 'README.en.md', 'CHANGELOG.md', '.gitignore'
)

foreach ($it in $items) {
  $src = Join-Path $root $it
  if (-not (Test-Path $src)) { Write-Warning "missing: $it (skip)"; continue }
  $dst = Join-Path $stage $it
  if (Test-Path $src -PathType Container) {
    robocopy $src $dst /E /NFL /NDL /NJH /NJS /NP | Out-Null
  } else {
    $dstDir = Split-Path -Parent $dst
    if (-not (Test-Path $dstDir)) { New-Item -ItemType Directory -Path $dstDir -Force | Out-Null }
    Copy-Item $src $dst
  }
}

if (Test-Path $zip) { Remove-Item $zip -Force }
Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zip
$size = [math]::Round((Get-Item $zip).Length / 1KB)
Write-Host "deploy package ready: $zip ($size KB)"
Write-Host 'target-machine steps: see deploy\README.md inside the zip'