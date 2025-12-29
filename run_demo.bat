@echo off
echo 🚧 Building Demo...

:: 1. Copy Dependencies from dist/ to examples/todo-list/
copy "dist\bridge.js" "examples\todo-list\" >nul
copy "dist\idb-keyval.js" "examples\todo-list\" >nul
copy "dist\wasm_exec.js" "examples\todo-list\" >nul

:: 2. Build the WASM
echo Building WASM Client...
set GOOS=js
set GOARCH=wasm
go build -o examples/todo-list/main.wasm ./cmd/client
:: Reset Env
set GOOS=windows
set GOARCH=amd64

echo ✅ Build Complete.
echo 🚀 Starting Server on :8080...

:: Start Server in new window
start "GoSync Server" cmd /k "go run ./cmd/server"

echo 🌍 Starting Web Host on :3000...
echo Visit http://localhost:3000 to see the demo.
cd examples/todo-list
python -m http.server 3000
