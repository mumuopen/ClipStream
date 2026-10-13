# Build the FULL (compiled) ClipStream deploy package for the target machine.
# Run on the dev machine. Output: .temp\clipstream-full-deploy.zip
# This package bundles EVERYTHING so the target machine is ready after unzip:
#   bin\clipstream.exe, bin\clipstream-daemon.exe
#   engines\biliup.exe, engines\ffmpeg.exe, engines\ffprobe.exe,
#   engines\yt-dlp.exe, engines\whisper-cli.exe, engines\ggml-small.bin
#   configs\, config.example.yaml (engines pre-pointed into ./engines/),
#   launcher .cmd + deploy instructions .txt (Chinese-named files, wildcard-copied)
# NOT bundled: data/ (DB & cookies), config.yaml (real keys) -> target fills them.

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$stage = Join-Path $root '.temp\full-stage'
$zip   = Join-Path $root '.temp\clipstream-full-deploy.zip'

if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Path (Join-Path $stage 'bin')   -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stage 'engines') -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $stage 'configs\platforms') -Force | Out-Null

# ---- 1) binaries ----
Copy-Item (Join-Path $root 'bin\clipstream.exe')        (Join-Path $stage 'bin\') -Force
Copy-Item (Join-Path $root 'bin\clipstream-daemon.exe') (Join-Path $stage 'bin\') -Force

# ---- 2) configs + platform files ----
Copy-Item (Join-Path $root 'configs\platforms\bilibili.yaml') (Join-Path $stage 'configs\platforms\') -Force
Copy-Item (Join-Path $root 'configs\platforms\douyin.yaml')   (Join-Path $stage 'configs\platforms\') -Force

# ---- 3) engine binaries (bundled, must be SELF-CONTAINED) ----
Copy-Item (Join-Path $root 'engines\biliup.exe')                    (Join-Path $stage 'engines\') -Force
Copy-Item 'D:\tools\ffmpeg\ffmpeg.exe'                              (Join-Path $stage 'engines\') -Force
Copy-Item 'D:\tools\ffmpeg\ffprobe.exe'                             (Join-Path $stage 'engines\') -Force
# yt-dlp: OFFICIAL STANDALONE exe (repo-local engines\yt-dlp.exe).
# NEVER ship the pip wrapper exe: it embeds the dev machine's python.exe path
# and dies instantly on any other host (exit status 1, zero output).
Copy-Item (Join-Path $root 'engines\yt-dlp.exe')                     (Join-Path $stage 'engines\') -Force
# whisper.cpp: plain build + its DLL chain. The exe ALONE cannot run:
# whisper-cli.exe -> whisper.dll -> ggml.dll -> ggml-cpu-<arch>.dll (+ ggml-base.dll)
$wh = Join-Path $root 'engines\whisper\plain\Release'
Copy-Item (Join-Path $wh 'whisper-cli.exe') (Join-Path $stage 'engines\') -Force
Copy-Item (Join-Path $wh 'whisper.dll')     (Join-Path $stage 'engines\') -Force
Copy-Item (Join-Path $wh 'ggml.dll')        (Join-Path $stage 'engines\') -Force
Copy-Item (Join-Path $wh 'ggml-base.dll')   (Join-Path $stage 'engines\') -Force
Copy-Item (Join-Path $wh 'ggml-cpu-*.dll') (Join-Path $stage 'engines\') -Force
# VC runtime app-local: a bare Windows Server may lack the VC++ 2015-2022 redist
foreach ($d in 'msvcp140.dll','vcruntime140.dll','vcruntime140_1.dll') {
  Copy-Item (Join-Path $env:SystemRoot "System32\$d") (Join-Path $stage 'engines\') -Force
}
Copy-Item (Join-Path $root 'engines\whisper\ggml-small.bin')        (Join-Path $stage 'engines\') -Force

# ---- 4) bundled config.example.yaml (engines pre-pointed to ./engines/) ----
$bundledConfig = @'
# ClipStream bundled config template (engines pre-pointed to ./engines/).
# Copy to config.yaml, then fill llm.api_key / notify.push_key / notify.url.
data_dir: ./data
proxy: ""

engines:
  ytdlp: "./engines/yt-dlp.exe"
  ffmpeg: "./engines/ffmpeg.exe"
  ffprobe: "./engines/ffprobe.exe"
  biliup: "./engines/biliup.exe"
  asr:
    binary: "./engines/whisper-cli.exe"
    model: "./engines/ggml-small.bin"
    language: auto

llm:
  base_url: "https://api.deepseek.com/v1"
  api_key: ""
  model: "deepseek-flash"
  thinking: "disabled"
  proxy: ""

web:
  listen: "127.0.0.1:8899"

publish:
  headless: false
  screenshots: true
  platforms_dir: "configs/platforms"

notify:
  pushme:
    enable: true
    url: ""
    push_key: ""

chrome:
  path: ""

live:
  check_interval: 60
  segment_time: 600
  auto_clip: true

retention:
  enabled: true
  segments_days: 7
  consumed_seg_days: 1
  min_free_percent: 10
  check_minutes: 60

auto_review:
  enabled: false
  min_score: 60
  hard_min_dur_sec: 15
  hard_max_dur_sec: 120
  banned_words: []
'@
$cfgPath = Join-Path $stage 'config.example.yaml'
[IO.File]::WriteAllText($cfgPath, $bundledConfig + "`r`n", [Text.UTF8Encoding]::new($false))

# ---- 5) launcher + instructions (wildcard copy keeps names, no safe literals) ----
$bootSrc = Join-Path $root '.temp\deploy-binary-stage'
Get-ChildItem (Join-Path $bootSrc '*.cmd') | ForEach-Object { Copy-Item -Path $_.FullName -Destination $stage -Force }
Get-ChildItem (Join-Path $bootSrc '*.txt') | ForEach-Object { Copy-Item -Path $_.FullName -Destination $stage -Force }

# ---- 6) zip ----
if (Test-Path $zip) { Remove-Item $zip -Force }
Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zip -CompressionLevel Optimal
$size = [math]::Round((Get-Item $zip).Length / 1MB)
Write-Host "full package ready: $zip ($size MB)"
Write-Host 'target-machine: unzip anywhere, copy config.example.yaml to config.yaml, fill keys, run the bundled .cmd launcher'