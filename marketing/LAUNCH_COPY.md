# GoSync Launch Copy

## 1. Hacker News (Show HN)

**Title:**
Show HN: GoSync – An offline-first sync engine for Go/WASM (SQLite + IndexedDB)

**Text:**
Hi HN,

I built an open-source sync engine that brings "Local-First" capabilities to Go web applications.

**The Problem:**
Most offline-first solutions (Firebase, PouchDB) force you into the JS ecosystem. If you are writing a Go backend, you often have to re-implement sync logic or settle for simple REST APIs that break when the user goes offline.

**The Solution:**
GoSync runs the **exact same Go code** in the browser (via WebAssembly) and on the server.
- **Client:** Go compiled to WASM, persisting to IndexedDB (via a small JS bridge).
- **Server:** Go with SQLite (or Postgres), persisting to disk.
- **Protocol:** Merkle Trees to detect diffs, and WebSockets to sync them.

If the server dies or the user goes offline, the client keeps working. When connectivity returns, it heals the state automatically.

**Repo:** https://github.com/HarshalPatel1972/GoSync
**WASM Demo:** (Link to your repo's demo instructions or a live link if you host it)

I'd love feedback on the Merkle Tree implementation and the WASM/JS bridge architecture.

---

## 2. Reddit (r/golang)

**Title:**
I built an Offline-First Sync Engine using Go, WASM, and Merkle Trees. Roast my code?

**Text:**
Hey Gophers,

I've spent the last week building **GoSync**, a library that lets you build "Local-First" apps using Go on both ends (Server + Browser WASM).

It solves the "offline problem" by treating the Browser's IndexedDB as the source of truth and syncing it with a Server SQLite DB using Merkle Trees to detect changes.

**The Tech Stack:**
- **Shared Logic:** The `Repository` interface is identical on client and server.
- **WASM Client:** Uses `syscall/js` to talk to a tiny `idb-keyval` bridge for persistence.
- **Concurrency:** Navigated the single-threaded WASM world (and its deadlock traps) by using custom Await implementations for JS Promises.

**The Code:**
https://github.com/HarshalPatel1972/GoSync

I'm particularly looking for feedback on:
1. Is my use of `syscall/js` for the JS bridge optimal, or should I be doing something cleaner?
2. The Merkle sync protocol – is there a more bandwidth-efficient way to handle the "Snapshot" phase?

Thanks!

---

## 3. Twitter / X / LinkedIn

**Text:**
Just shipped GoSync 🚀
An offline-first sync engine for Go.

✅ Go on the Server
✅ Go in the Browser (WASM)
✅ IndexedDB Persistence
✅ Merkle Tree Sync

No vendor lock-in. Just code.

Check it out: https://github.com/HarshalPatel1972/GoSync
#golang #webassembly #opensource #offlinefirst
