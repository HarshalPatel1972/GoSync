build-wasm:
	set GOOS=js
	set GOARCH=wasm
	go build -o dist/main.wasm ./cmd/client
