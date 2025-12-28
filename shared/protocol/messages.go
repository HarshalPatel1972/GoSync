package protocol

type Message struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

const (
	MessageTypeHashCheck = "HASH_CHECK"
	MessageTypeSyncData  = "SYNC_DATA"
)

type SyncState struct {
	RootHash string `json:"root_hash"`
	Count    int    `json:"count"`
}
