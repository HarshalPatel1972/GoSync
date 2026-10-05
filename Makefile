.PHONY: test build sdk demo e2e

test:
	go test -race -count=1 ./...

build:
	go build -o bin/gosync-server ./cmd/gosync-server

sdk:
	node sdk/js/build.mjs --out examples/todo/gosync

# Runs the todo demo at http://localhost:8080 with insecure demo auth.
demo: sdk
	GOSYNC_INSECURE_DEV_AUTH=true GOSYNC_STATIC_DIR=examples/todo GOSYNC_LOG_FORMAT=text \
		GOSYNC_DATABASE_URL=demo.db go run ./cmd/gosync-server
