# Contributing to GoSync

Thanks for helping. GoSync syncs other people's data, so correctness comes first: every change
needs a test that would fail without it.

The website and documentation live in a separate repository,
[GoSync-zero](https://github.com/HarshalPatel1972/GoSync-zero). This repository is the engine:
server, protocol, Go client and browser SDK.

## Ways to help

- **Report a bug:** open an issue with what you did, what you expected, what happened, and the
  GoSync version (server and `@harshalpatel2868/gosync-client`). Sync bugs are much easier to fix
  with the sequence of writes and connections that led to them.
- **Say what you're building:** issues describing real use cases help decide the roadmap.
- **Send a pull request:** for anything larger than a small fix, open an issue first so we can
  agree on the approach.
- **Security issues:** please don't open a public issue. Use GitHub's private vulnerability
  reporting (Security tab → "Report a vulnerability").

## Development setup

Requires Go 1.26+ and Node 18+.

```bash
git clone https://github.com/HarshalPatel1972/GoSync.git
cd GoSync
go test ./...                                   # Go tests (SQLite)
node sdk/js/build.mjs --out examples/todo/gosync # build the browser SDK into the demo
```

Run the demo with `make demo`, or see [Try the demo](README.md#try-the-demo) for the commands
without `make`.

## Tests

| What | Command |
|---|---|
| Go tests with the race detector | `go test -race -count=1 ./...` |
| Also against PostgreSQL | `GOSYNC_TEST_POSTGRES=postgres://… go test -race ./...` |
| IndexedDB bridge rules | `node sdk/js/test/bridge.mjs` |
| SDK end-to-end (needs a demo server on :8090) | `node sdk/js/test/e2e.mjs` |
| Real browsers: Chromium, Firefox, WebKit | `node sdk/js/test/browser.mjs` |
| Bundlers: Vite, Next.js (Turbopack, webpack) | `GOSYNC_SERVER_BIN=./gosync-server node sdk/js/test/bundlers.mjs` |
| Load | `go run ./cmd/gosync-loadtest -help` |

The browser tests need `npm install` in `sdk/js` and `npx playwright install`. CI runs all of
these on every pull request, plus a load test with two server instances on PostgreSQL.

## Guidelines

- **Merge rules must match.** Last-writer-wins logic exists in Go (`client/store.go`) and in the
  browser's IndexedDB bridge (`sdk/js/gosync.js`). Change both together, and extend
  `sdk/js/test/bridge.mjs` to keep them in step.
- **Protocol changes** need an update to [PROTOCOL.md](PROTOCOL.md) and must stay compatible
  with deployed clients, or bump the protocol version.
- **Schema changes** are new migrations appended in `store/sqlstore`; never edit a released one.
- **Docs and claims** must match the code. If you change behaviour, update the README,
  `docs/OPERATIONS.md` and the website docs.
- Run `gofmt` and keep `go vet ./...` clean.

## License

By contributing, you agree that your contributions are licensed under the MIT License.
