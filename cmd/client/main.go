package main

import (
	"fmt"
	"syscall/js"
	"time"

	"github.com/HarshalPatel1972/GoSync/shared/models"
	"github.com/HarshalPatel1972/GoSync/shared/repository"
)

var repo *repository.MemoryRepository

func main() {
	fmt.Println("GoSync WASM initialized")
	repo = repository.NewMemoryRepository()

	js.Global().Set("addItemToStore", js.FuncOf(addItemToStore))

	select {} // Keep the Go program running
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

	hash, _ := repo.GetStateHash()
	fmt.Printf("Item added. New State Hash: %s\n", hash)
	return nil
}
