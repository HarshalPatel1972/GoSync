package models

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

type Item struct {
	ID        string `json:"id" gorm:"primaryKey"`
	Content   string `json:"content"`
	IsDeleted bool   `json:"is_deleted"`
	UpdatedAt int64  `json:"updated_at" gorm:"autoUpdateTime:false"`
}

func (i Item) CalculateHash() string {
	data := fmt.Sprintf("%s:%s:%t:%d", i.ID, i.Content, i.IsDeleted, i.UpdatedAt)
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}
