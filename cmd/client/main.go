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

var repo *repository.MemoryRepository
var jsWebSocket js.Value

func main() {
	fmt.Println("GoSync WASM initialized")
	repo = repository.NewMemoryRepository()

	js.Global().Set("addItemToStore", js.FuncOf(addItemToStore))

	// Connect to WebSocket
	connectWebSocket()

	select {} // Keep the Go program running
}

func connectWebSocket() {
	ws := js.Global().Get("WebSocket").New("ws://localhost:8080/ws")

	ws.Set("onopen", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		fmt.Println("WebSocket connection opened")
		sendHashCheck(ws)
		return nil
	}))

	ws.Set("onmessage", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		event := args[0]
		dataStr := event.Get("data").String()
		
		var msg protocol.Message
		err := json.Unmarshal([]byte(dataStr), &msg)
		if err != nil {
			fmt.Println("Error unmarshalling message:", err)
			return nil
		}

		switch msg.Type {
		case protocol.MessageTypeSyncNeeded:
			fmt.Println("Server requested sync. Uploading data...")
			uploadData(ws)
		case protocol.MessageTypeHashCheck:
			var serverState protocol.SyncState
			json.Unmarshal([]byte(msg.Payload), &serverState)
			localHash, _ := repo.GetStateHash()
			if serverState.RootHash == localHash {
				fmt.Println("SYNCED! Server matches local state.")
			} else {
				fmt.Println("Server hash mismatch:", serverState.RootHash)
			}
		default:
			fmt.Printf("Received unknown message type: %s\n", msg.Type)
		}

		return nil
	}))

	jsWebSocket = ws
}

func uploadData(ws js.Value) {
	items, _ := repo.GetAllItems()
	syncData := protocol.SyncData{
		Items: items,
	}
	
	payload, _ := json.Marshal(syncData)
	msg := protocol.Message{
		Type:    protocol.MessageTypeSyncUpload,
		Payload: string(payload),
	}

	jsonMsg, _ := json.Marshal(msg)
	ws.Call("send", string(jsonMsg))
	fmt.Printf("Uploaded %d items to server\n", len(items))
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
		return nil
	}

	// Trigger hash check after adding item
	if !jsWebSocket.IsUndefined() {
		sendHashCheck(jsWebSocket)
	}

	return nil
}
