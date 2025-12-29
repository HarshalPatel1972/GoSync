package main

import (
	"encoding/json"
	"fmt"
	"syscall/js"
	"time"

	"github.com/HarshalPatel1972/GoSync/shared/models"
	"github.com/HarshalPatel1972/GoSync/shared/protocol"
	"github.com/HarshalPatel1972/GoSync/shared/repository"
)

var repo repository.Repository
var jsWebSocket js.Value

func main() {
	fmt.Println("GoSync WASM initialized")
	// Use BrowserRepository (LocalStorage/IndexedDB) instead of Memory
	repo = NewBrowserRepository()

	// Perform initial load in a goroutine
	go func() {
		items, err := repo.GetAllItems()
		if err != nil {
			fmt.Println("Error loading items:", err)
			return
		}
		fmt.Printf("Loaded %d items from IndexedDB\n", len(items))
	}()

	js.Global().Set("addItemToStore", js.FuncOf(addItemToStore))

	// Connect to WebSocket (also handles async logic internally if needed)
	connectWebSocket()

	// Keep the Go program running correctly
	c := make(chan struct{})

    // Use JS to drive the heartbeat. This prevents Go from thinking it's deadlocked,
    // and prevents the browser from freezing (since Go yields to JS fully).
    keepAlive := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
        return nil
    })
    js.Global().Call("setInterval", keepAlive, 5000) // Wake up every 5s

	<-c
}

func connectWebSocket() {
	ws := js.Global().Get("WebSocket").New("ws://localhost:8080/ws")

	ws.Set("onopen", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		fmt.Println("WebSocket connection opened")
		go sendHashCheck(ws)
		return nil
	}))

	ws.Set("onmessage", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		// Handle message inside goroutine to safely await DB calls
		event := args[0]
		dataStr := event.Get("data").String()
		go func() {
			var msg protocol.Message
			err := json.Unmarshal([]byte(dataStr), &msg)
			if err != nil {
				fmt.Println("Error unmarshalling message:", err)
				return
			}

			switch msg.Type {
			case protocol.MessageTypeRequestSnapshot:
				fmt.Println("Server requested snapshot. Sending Snapshot...")
				sendSnapshot(ws)
			case protocol.MessageTypeHashCheck:
				var serverState protocol.SyncState
				json.Unmarshal([]byte(msg.Payload), &serverState)
				localHash, _ := repo.GetStateHash()
				if serverState.RootHash == localHash {
					fmt.Println("SYNCED! Server matches local state.")
				} else {
					fmt.Println("Server hash mismatch:", serverState.RootHash)
					fmt.Println("Requesting snapshot from server...")
					sendRequestSnapshot(ws)
				}
			case protocol.MessageTypeSnapshotData:
				fmt.Println("Received snapshot from server.")
				var snapshot protocol.SnapshotPayload
				json.Unmarshal([]byte(msg.Payload), &snapshot)
				
				count := 0
				for _, item := range snapshot.Items {
					err := repo.PutItem(item)
					if err == nil {
						count++
					} else {
						fmt.Printf("Error saving item %s: %v\n", item.ID, err)
					}
				}
				fmt.Printf("Applied %d items from server snapshot.\n", count)
				// Re-verify sync status
				go sendHashCheck(ws)
			default:
				fmt.Printf("Received unknown message type: %s\n", msg.Type)
			}
		}()
		return nil
	}))

	jsWebSocket = ws
}

func sendSnapshot(ws js.Value) {
	items, _ := repo.GetAllItems()
	snapshot := protocol.SnapshotPayload{
		Items: items,
	}
	
	payload, _ := json.Marshal(snapshot)
	msg := protocol.Message{
		Type:    protocol.MessageTypeSnapshotData,
		Payload: string(payload),
	}

	jsonMsg, _ := json.Marshal(msg)
	ws.Call("send", string(jsonMsg))
	fmt.Printf("Sent Snapshot with %d items\n", len(items))
}

func sendHashCheck(ws js.Value) {
	hash, _ := repo.GetStateHash()
	items, _ := repo.GetAllItems()

	state := protocol.SyncState{
		RootHash: hash,
		Count:    len(items),
	}

	payload, _ := json.Marshal(state)
	msg := protocol.Message{
		Type:    protocol.MessageTypeHashCheck,
		Payload: string(payload),
	}

	jsonMsg, _ := json.Marshal(msg)
	ws.Call("send", string(jsonMsg))
	fmt.Printf("Sent HASH_CHECK: %s\n", hash)
}

func addItemToStore(this js.Value, args []js.Value) interface{} {
	if len(args) < 1 {
		fmt.Println("No content provided")
		return nil
	}
	content := args[0].String()
	
	// Run in goroutine to allow Await (channel block) to work without deadlocking main thread
	go func() {
		// Create a new item
		item := models.Item{
			// In a real app, use a UUID. Here using timestamp for simplicity in this phase.
			ID:        fmt.Sprintf("%d", time.Now().UnixNano()), 
			Content:   content,
			IsDeleted: false,
			UpdatedAt: time.Now().Unix(),
		}

		err := repo.PutItem(item)
		if err != nil {
			fmt.Printf("Error adding item: %s\n", err)
			return
		}

		// Trigger hash check after adding item
		if !jsWebSocket.IsUndefined() {
			sendHashCheck(jsWebSocket)
		}
	}()

	return nil
}

func sendRequestSnapshot(ws js.Value) {
	msg := protocol.Message{
		Type: protocol.MessageTypeRequestSnapshot,
	}
	jsonMsg, _ := json.Marshal(msg)
	ws.Call("send", string(jsonMsg))
	fmt.Println("Sent REQUEST_SNAPSHOT")
}
