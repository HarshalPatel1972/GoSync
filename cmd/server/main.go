package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/HarshalPatel1972/GoSync/shared/protocol"
	"github.com/HarshalPatel1972/GoSync/shared/repository"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for this demo
	},
}

var repo *repository.MemoryRepository

func main() {
	repo = repository.NewMemoryRepository()

	http.HandleFunc("/ws", handleConnections)

	fmt.Println("Server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Error upgrading connection: %v", err)
		return
	}
	defer ws.Close()

	fmt.Println("Client connected")

	for {
		var msg protocol.Message
		err := ws.ReadJSON(&msg)
		if err != nil {
			log.Printf("Error reading json: %v", err)
			break
		}

		if msg.Type == protocol.MessageTypeHashCheck {
			handleHashCheck(ws, msg)
		} else if msg.Type == protocol.MessageTypeSnapshotData {
			handleSnapshotData(ws, msg)
		}
	}
}

func handleHashCheck(ws *websocket.Conn, msg protocol.Message) {
	// Parse client state
	var clientState protocol.SyncState
	err := json.Unmarshal([]byte(msg.Payload), &clientState)
	if err != nil {
		log.Printf("Error unmarshalling payload: %v", err)
		return
	}

	// Calculate local state
	serverHash, _ := repo.GetStateHash()

	fmt.Printf("Use Hash: %s | Server Hash: %s\n", clientState.RootHash, serverHash)

	if clientState.RootHash != serverHash {
		fmt.Println("Hash mismatch! Requesting Snapshot...")
		// Request Snapshot
		request := protocol.Message{
			Type: protocol.MessageTypeRequestSnapshot,
		}
		ws.WriteJSON(request)
	} else {
		fmt.Println("In Sync")
	}
}

func handleSnapshotData(ws *websocket.Conn, msg protocol.Message) {
	var snapshot protocol.SnapshotPayload
	err := json.Unmarshal([]byte(msg.Payload), &snapshot)
	if err != nil {
		log.Printf("Error unmarshalling snapshot data: %v", err)
		return
	}

	fmt.Printf("Received %d items.\n", len(snapshot.Items))

	for _, item := range snapshot.Items {
		repo.PutItem(item)
	}

	// Recalculate hash and confirm sync
	newHash, _ := repo.GetStateHash()
	fmt.Printf("Synced! New Hash: %s\n", newHash)

	// Send back HASH_CHECK to confirm to client
	state := protocol.SyncState{
		RootHash: newHash,
		Count:    len(snapshot.Items),
	}
	payload, _ := json.Marshal(state)
	
	response := protocol.Message{
		Type:    protocol.MessageTypeHashCheck,
		Payload: string(payload),
	}
	ws.WriteJSON(response)
}
