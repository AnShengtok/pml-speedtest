sidecar 必须按 <名称>-<rustc 目标三元组>.exe 命名，例如：
  pml-engine-x86_64-pc-windows-msvc.exe
构建命令（在本目录执行）：
  set GOOS=windows& set GOARCH=amd64& set CGO_ENABLED=0
  go build -trimpath -o src-tauri\binaries\pml-engine-x86_64-pc-windows-msvc.exe .\cmd\engine
