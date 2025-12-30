# GoSync Protocol Specification

**Version:** 1.0  
**Status:** Stable  
**Last Updated:** December 2024

---

## 1. Abstract

GoSync is an offline-first synchronization protocol designed for applications that need to work seamlessly both online and offline. It synchronizes state between a browser-based client (IndexedDB) and a server-side database (SQLite/PostgreSQL) using cryptographic hashing for efficient change detection.

---

## 2. Architecture

### 2.1 Topology
**Client-Server (Star Topology)**

```
        ┌─────────┐
        │ Server  │
        │ (SQLite)│
        └────┬────┘
             │
    ┌────────┼────────┐
    │        │        │
┌───▼──┐ ┌───▼──┐ ┌───▼──┐
│Client│ │Client│ │Client│
│ (IDB)│ │ (IDB)│ │ (IDB)│
└──────┘ └──────┘ └──────┘
```

All clients connect to a central server. The server acts as the authoritative sync point. Clients do not communicate directly with each other.

### 2.2 Transport Layer
| Property | Value |
|----------|-------|
| Protocol | WebSocket (RFC 6455) |
| Default Port | 8080 |
| Connection | Persistent, full-duplex |
| Reconnection | Client-initiated with exponential backoff (recommended) |

### 2.3 Serialization
| Property | Value |
|----------|-------|
| Format | JSON (UTF-8 encoded) |
| Framing | WebSocket text frames |
| Schema | Implicit (no external schema required) |

---

## 3. Data Model

### 3.1 The `Item` Entity

All syncable data is represented as an `Item`:

| Field | Type | Size | Description |
|-------|------|------|-------------|
| `id` | String | Variable | Unique identifier. Default: Unix nanosecond timestamp. |
| `content` | String | Variable | Application payload (the actual data). |
| `is_deleted` | Boolean | 1 byte | Tombstone flag. `true` = soft-deleted. |
| `updated_at` | Int64 | 8 bytes | Unix timestamp in seconds. |

### 3.2 Item Hash Calculation

Each item produces a deterministic hash for change detection:

```
Hash = SHA-256( id + ":" + content + ":" + is_deleted + ":" + updated_at )
```

**Example:**
```
Input:  "1703961600000:Buy milk:false:1703961600"
Output: "a3f2b8c9d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1"
```

### 3.3 Root Hash (State Hash)

The Root Hash represents the entire dataset state:

```go
func GenerateRootHash(items []Item) string {
    // 1. Sort items by ID for deterministic ordering
    sort.Slice(items, func(i, j int) bool {
        return items[i].ID < items[j].ID
    })
    
    // 2. Concatenate all individual hashes
    var combined string
    for _, item := range items {
        combined += item.CalculateHash()
    }
    
    // 3. Return hash of combined string
    return SHA256(combined)
}
```

**Property:** If any single item differs (added, modified, deleted), the Root Hash will differ.

---

## 4. Message Protocol

### 4.1 Message Envelope

All messages follow this structure:

```json
{
  "type": "<MESSAGE_TYPE>",
  "payload": "<JSON_STRING>"
}
```

### 4.2 Message Types

| Code | Type | Direction | Description |
|------|------|-----------|-------------|
| `0x01` | `HASH_CHECK` | Bidirectional | State comparison request/response |
| `0x02` | `REQUEST_SNAPSHOT` | Bidirectional | Request full dataset from peer |
| `0x03` | `SNAPSHOT_DATA` | Bidirectional | Full dataset payload |

### 4.3 Message Payloads

#### HASH_CHECK (0x01)
```json
{
  "type": "HASH_CHECK",
  "payload": "{\"root_hash\":\"<SHA256>\",\"count\":<INT>}"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `root_hash` | String (64 hex chars) | SHA-256 of entire dataset |
| `count` | Integer | Number of items in dataset |

#### REQUEST_SNAPSHOT (0x02)
```json
{
  "type": "REQUEST_SNAPSHOT",
  "payload": ""
}
```
No payload. Simply requests the peer to send all items.

#### SNAPSHOT_DATA (0x03)
```json
{
  "type": "SNAPSHOT_DATA",
  "payload": "{\"items\":[<ITEM>, <ITEM>, ...]}"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `items` | Array<Item> | Complete list of all items |

---

## 5. Synchronization Flow

### 5.1 Initial Handshake

```
┌────────┐                                    ┌────────┐
│ Client │                                    │ Server │
└───┬────┘                                    └───┬────┘
    │                                             │
    │ ──────── [1] HASH_CHECK ──────────────────▶ │
    │          {root_hash: "abc...", count: 5}    │
    │                                             │
    │          [Server compares hashes]           │
    │                                             │
    │ ◀─────── [2] REQUEST_SNAPSHOT ───────────── │  (if hashes differ)
    │                                             │
    │ ──────── [3] SNAPSHOT_DATA ───────────────▶ │
    │          {items: [...]}                     │
    │                                             │
    │          [Server saves, recalculates hash]  │
    │                                             │
    │ ◀─────── [4] HASH_CHECK ──────────────────── │
    │          {root_hash: "abc...", count: 5}    │
    │                                             │
    │          [Client compares: MATCH]           │
    │                                             │
    │              ✓ SYNCHRONIZED                 │
    └─────────────────────────────────────────────┘
```

### 5.2 Bidirectional Sync

The protocol supports healing in both directions:

| Scenario | Flow |
|----------|------|
| Server is behind | Server requests snapshot from Client |
| Client is behind | Client requests snapshot from Server |
| Both in sync | No data exchanged after HASH_CHECK |

### 5.3 Change Propagation

When a client adds/modifies an item:

1. Item is saved to local IndexedDB immediately
2. Client sends `HASH_CHECK` to server
3. If mismatch, snapshot exchange occurs
4. Both sides end up synchronized

---

## 6. Conflict Resolution

### 6.1 Strategy: Snapshot-Based Overwrite

**Current Implementation:** When a snapshot is received, all items are saved/overwritten. The side that sends its data last "wins."

### 6.2 Tie-Breaking

In the current implementation, conflicts are resolved by:
1. The party receiving the snapshot overwrites its local state
2. If both parties send simultaneously (race condition), the last write persists

### 6.3 Consistency Guarantee

| Property | Value |
|----------|-------|
| Model | Eventual Consistency |
| Guarantee | All connected clients will converge to the same state |
| Latency | Typically < 100ms on stable connections |

### 6.4 Future: Per-Item LWW

Planned improvement: Compare `updated_at` timestamps per-item before overwriting:
```go
if incoming.UpdatedAt > existing.UpdatedAt {
    save(incoming)
}
```

---

## 7. Error Handling

### 7.1 Error Conditions

| Condition | Behavior |
|-----------|----------|
| WebSocket disconnect | Client should reconnect and resend HASH_CHECK |
| Malformed JSON | Message is logged and ignored |
| Unknown message type | Message is logged and ignored |
| Database write failure | Error logged, sync continues with other items |

### 7.2 Recovery

| Scenario | Recovery Strategy |
|----------|-------------------|
| Client crashes mid-sync | On restart, full HASH_CHECK restarts sync |
| Server crashes mid-sync | Client retries on reconnect |
| Partial snapshot received | WebSocket framing ensures complete messages |

### 7.3 Idempotency

All operations are idempotent:
- Receiving the same item twice results in an overwrite (no duplicates)
- Receiving the same HASH_CHECK twice triggers the same comparison

---

## 8. Offline Behavior

### 8.1 Write Path (Offline)

```
User Action ──▶ Save to IndexedDB ──▶ Done (Instant)
                     │
                     └──▶ [No network request if offline]
```

### 8.2 Read Path (Offline)

```
User Request ──▶ Read from IndexedDB ──▶ Return Data (Instant)
```

### 8.3 Reconnection

```
WebSocket connects ──▶ onopen fires ──▶ Client sends HASH_CHECK
                                              │
                                              ▼
                                     Snapshot exchange if needed
```

---

## 9. Security Considerations

### 9.1 Current Implementation

| Aspect | Status |
|--------|--------|
| Transport Encryption | ❌ Not implemented (uses `ws://`) |
| Authentication | ❌ Not implemented |
| Authorization | ❌ Not implemented |

### 9.2 Recommendations for Production

| Aspect | Recommendation |
|--------|----------------|
| Transport | Use `wss://` (WebSocket Secure) with TLS |
| Authentication | JWT tokens in WebSocket URL or first message |
| Authorization | Validate user ownership of items before sync |

---

## 10. Persistence Layer

### 10.1 Client (Browser)

| Property | Value |
|----------|-------|
| Storage Engine | IndexedDB |
| Library | `idb-keyval` (via JS bridge) |
| Database Name | `keyval-store` (default) |

### 10.2 Server

| Property | Value |
|----------|-------|
| Storage Engine | SQLite (default) or PostgreSQL |
| ORM | GORM |
| File | `server.db` (SQLite) |

---

## 11. Implementation Notes

### 11.1 Go/WASM Considerations

The client runs as Go compiled to WebAssembly. Key adaptations:
- JavaScript bridge for IndexedDB access
- Promise-to-channel adapter for async operations
- Goroutine-based message handling to prevent deadlocks

### 11.2 Why Not Full Merkle Tree Traversal?

The current implementation uses Root Hash comparison + Full Snapshot exchange instead of tree traversal because:
1. Simpler implementation
2. Sufficient for datasets < 10,000 items
3. Guaranteed correctness

Full Merkle Tree traversal (O(log n) sync) is planned for v2.

---

## 12. Wire Format Examples

### Complete Sync Session

**Step 1: Client sends HASH_CHECK**
```json
{"type":"HASH_CHECK","payload":"{\"root_hash\":\"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\",\"count\":0}"}
```

**Step 2: Server requests snapshot (hashes differ)**
```json
{"type":"REQUEST_SNAPSHOT","payload":""}
```

**Step 3: Client sends all items**
```json
{"type":"SNAPSHOT_DATA","payload":"{\"items\":[{\"id\":\"1703961600000\",\"content\":\"Buy milk\",\"is_deleted\":false,\"updated_at\":1703961600}]}"}
```

**Step 4: Server confirms sync**
```json
{"type":"HASH_CHECK","payload":"{\"root_hash\":\"a1b2c3d4e5f6...\",\"count\":1}"}
```

---

## Appendix A: Glossary

| Term | Definition |
|------|------------|
| Root Hash | SHA-256 hash representing entire dataset state |
| Snapshot | Complete list of all items in the dataset |
| Tombstone | Soft-deleted item (is_deleted = true) |
| WASM | WebAssembly - allows Go to run in browsers |
| IDB | IndexedDB - browser-side database |

---

*End of Protocol Specification*
