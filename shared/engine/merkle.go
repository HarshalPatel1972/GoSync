package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/HarshalPatel1972/GoSync/shared/models"
)

func GenerateRootHash(items []models.Item) string {
	if len(items) == 0 {
		emptyHash := sha256.Sum256([]byte(""))
		return hex.EncodeToString(emptyHash[:])
	}

	// Sort items by ID for determinism
	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})

	var hashStrings []string
	for _, item := range items {
		hashStrings = append(hashStrings, item.CalculateHash())
	}

	combined := strings.Join(hashStrings, "")
	rootHash := sha256.Sum256([]byte(combined))
	return hex.EncodeToString(rootHash[:])
}
