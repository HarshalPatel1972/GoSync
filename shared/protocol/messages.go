package protocol

import "github.com/HarshalPatel1972/GoSync/shared/models"

type Message struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

const (
	MessageTypeHashCheck  = "HASH_CHECK"
	MessageTypeSyncData   = "SYNC_DATA"
	MessageTypeSyncNeeded = "SYNC_NEEDED"
	MessageTypeSyncUpload = "SYNC_UPLOAD"
)

type SyncState struct {
	RootHash string `json:"root_hash"`
	Count    int    `json:"count"`
}

type SyncData struct {
	Items []models.Item `json:"items"`
}

