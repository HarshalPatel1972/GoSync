#!/bin/bash
set -e

echo "🚧 Building Demo..."

# 1. Copy Dependencies
cp dist/bridge.js examples/todo-list/
cp dist/idb-keyval.js examples/todo-list/
cp dist/wasm_exec.js examples/todo-list/

# 2. Build the WASM client
echo "Building WASM Client..."
GOOS=js GOARCH=wasm go build -o examples/todo-list/main.wasm ./cmd/client

echo "✅ Build Complete."
echo "🚀 Starting Server on :8080..."
echo "🌍 Starting Web Host on :3000..."

# Start Go Server in background
go run ./cmd/server &
SERVER_PID=$!

# Ensure server is killed on exit
trap "kill $SERVER_PID" EXIT

# Start Web Server
echo "Visit http://localhost:3000 to see the demo."
cd examples/todo-list
python3 -m http.server 3000
