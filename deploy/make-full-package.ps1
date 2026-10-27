# Build a CLEAN ClipStream deploy package from temp_server\ (the staging dir the
# operator drags to the server). Output: .temp\clipstream-clean\  (+ .zip with -Zip)
#
# Why this script exists / what it deliberately EXCLUDES:
#   config.yaml : holds real LLM/PushMe keys -> copy it over deliberately, never bundle
#   data\       : local test residue, DB, cookies -> never ship to the server
#   log\        : local run logs -> would overwrite the server's production log
#
# 2026-10-28 fixes (the old version could not even finish):
#   1. It took exe from the REPO-ROOT bin\ -- that is a stale build
#      (measured: root bin\clipstream.exe version = 0.4.3, while temp_server\bin = 0.6.2).
#   2. It read launcher scripts from .temp\deploy-binary-stage, which no longer exists;
#      with $ErrorActionPreference='Stop' the script died at step 5 every time.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File deploy\make-full-package.ps1
#   powershell -ExecutionPolicy Bypass -File deploy\make-full-package.ps1 -Zip
#   powershell -ExecutionPolicy Bypass -File deploy\make-full-package.ps1 -IncludeConfig
#     (-IncludeConfig bundles the real config WITH keys: offline personal use only)

param(
  [switch]$Zip,
  [switch]$IncludeConfig
)

$ErrorActionPreference = 'Stop'
$root    = Split-Path -Parent $PSScriptRoot
$src     = Join-Path $root 'temp_server'
$stage   = Join-Path $root '.temp\clipstream-clean'
$zipPath = Join-Path $root '.temp\clipstream-clean.zip'

if (-not (Test-Path (Join-Path $src 'bin\clipstream.exe'))) {
  throw "missing $src\bin\clipstream.exe -- put the current pair of exes in temp_server\bin first"
}

if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Path $stage -Force | Out-Null

function Copy-Tree([string]$from, [string]$to) {
  if (-not (Test-Path $from)) { Write-Warning "missing: $from (skip)"; return }
  New-Item -ItemType Directory -Path $to -Force | Out-Null
  robocopy $from $to /E /NFL /NDL /NJH /NJS /NP | Out-Null
  if ($LASTEXITCODE -ge 8) { throw "robocopy failed ($LASTEXITCODE): $from -> $to" }
}

# 1) binaries (single source of truth: temp_server\bin)
Copy-Tree (Join-Path $src 'bin') (Join-Path $stage 'bin')

# 2) bundled engines (self-contained: target machine needs zero installs)
Copy-Tree (Join-Path $src 'engines') (Join-Path $stage 'engines')

# 3) platform selector configs
Copy-Tree (Join-Path $src 'configs') (Join-Path $stage 'configs')

# 4) launchers + deploy notes (Chinese file names: wildcard copy, no literals)
Get-ChildItem $src -File | Where-Object { $_.Extension -in '.cmd', '.txt' } |
  ForEach-Object { Copy-Item $_.FullName $stage -Force }

# 5) data / log: empty skeleton only, never content
foreach ($d in 'data', 'data\jobs', 'data\live\queue', 'data\accounts', 'log') {
  New-Item -ItemType Directory -Path (Join-Path $stage $d) -Force | Out-Null
}

# 6) config.yaml: not bundled by default
if ($IncludeConfig) {
  Copy-Item (Join-Path $src 'config.yaml') (Join-Path $stage 'config.yaml') -Force
  Write-Warning 'config.yaml INCLUDED (contains real keys) -- do not share, do not commit'
} else {
  Write-Host 'config.yaml NOT bundled (contains real LLM/PushMe keys): copy it manually when deploying'
}

# 7) verify the packaged binary version (prevents shipping a stale build again)
$verOut = & (Join-Path $stage 'bin\clipstream.exe') version 2>&1
Write-Host "package ready: $stage"
Write-Host "binary version: $verOut"

if ($Zip) {
  if (Test-Path $zipPath) { Remove-Item $zipPath -Force }
  # engines are ~750MB; Fastest is good enough and much quicker
  Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zipPath -CompressionLevel Fastest
  $mb = [math]::Round((Get-Item $zipPath).Length / 1MB, 1)
  Write-Host "zip: $zipPath ($mb MB)"
}

Write-Host ''
Write-Host 'Before dragging to the server:'
Write-Host '  - data\ and log\ must be EMPTY (they are after this script), otherwise they overwrite server DB/log'
Write-Host '  - copy config.yaml deliberately; if the server publish rhythm was already tuned,'
Write-Host '    do NOT overwrite it wholesale (it would silently revert to the sprint profile)'
