# 打流测试 · Tauri 壳一键出包（2026-09-11 实测通过：安装包 4.3 MB）
# 工具链按本机长期约定全部装在 D 盘：Rust=D:\dev\rust，MSVC=D:\VS\BuildTools，Go/Node=D:\qwrt\tools。
# 用法：powershell -NoProfile -ExecutionPolicy Bypass -File bootstrap.ps1
$ErrorActionPreference = 'Stop'

$env:RUSTUP_HOME = 'D:\dev\rust\.rustup'
$env:CARGO_HOME = 'D:\dev\rust\.cargo'
$env:npm_config_cache = 'D:\dev\npm-cache'
$env:PATH = 'D:\dev\rust\.cargo\bin;D:\qwrt\tools\node-v20.19.0-win-x64;D:\qwrt\tools\go\bin;' + $env:PATH
$env:CGO_ENABLED = '0'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$env:CARGO_TERM_COLOR = 'never'

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$repo = Split-Path -Parent $here
$sidecar = Join-Path $here 'src-tauri\binaries\pml-engine-x86_64-pc-windows-msvc.exe'

# 1) 交叉编译 headless 引擎为 sidecar（纯 Go，无需 CGO）
Write-Output '[1/3] build sidecar'
Push-Location $repo
go build -trimpath -ldflags '-s -w' -o $sidecar .\cmd\engine
$rc = $LASTEXITCODE
Pop-Location
if ($rc -ne 0) { throw 'engine sidecar build failed' }

# 2) 装 Tauri CLI（已装则跳过）
Write-Output '[2/3] prepare @tauri-apps/cli'
Push-Location $here
if (-not (Test-Path 'node_modules\@tauri-apps\cli')) {
  npm install --no-fund --no-audit -D '@tauri-apps/cli@2'
  if ($LASTEXITCODE -ne 0) { Pop-Location; throw 'npm install failed' }
}

# 3) 出包（首次编译约 5-15 分钟）
Write-Output '[3/3] tauri build'
npx tauri build
$rc = $LASTEXITCODE
Pop-Location
if ($rc -ne 0) { throw 'tauri build failed' }

Get-ChildItem (Join-Path $here 'src-tauri\target\release\bundle\nsis\*.exe') | ForEach-Object {
  $md5 = (Get-FileHash -Algorithm MD5 $_.FullName).Hash.ToLower()
  'SETUP  {0}  {1:N1} MB  md5={2}' -f $_.Name, ($_.Length / 1MB), $md5
}
