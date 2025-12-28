package protocol

import "github.com/HarshalPatel1972/GoSync/shared/models"

type Message struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

const (
	MessageTypeHashCheck      = "HASH_CHECK"
	MessageTypeRequestSnapshot = "REQUEST_SNAPSHOT"
	MessageTypeSnapshotData    = "SNAPSHOT_DATA"
)

type SyncState struct {
	RootHash string `json:"root_hash"`
	Count    int    `json:"count"`
}

type SnapshotPayload struct {
	Items []models.Item `json:"items"`
}

