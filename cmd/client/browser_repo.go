package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"syscall/js"

	"github.com/HarshalPatel1972/GoSync/shared/engine"
	"github.com/HarshalPatel1972/GoSync/shared/models"
)

// BrowserRepository uses localStorage for simplicity in this MVP.
// Ideally, this should use IndexedDB for large datasets.
type BrowserRepository struct {
	storeName string
}

func NewBrowserRepository() *BrowserRepository {
	return &BrowserRepository{
		storeName: "gosync_items",
	}
}

func (r *BrowserRepository) PutItem(item models.Item) error {
	currentItemMap := r.loadMap()
	currentItemMap[item.ID] = item
	return r.saveMap(currentItemMap)
}

func (r *BrowserRepository) GetItem(id string) (models.Item, error) {
	currentItemMap := r.loadMap()
	item, ok := currentItemMap[id]
	if !ok {
		return models.Item{}, errors.New("item not found")
	}
	return item, nil
}

func (r *BrowserRepository) GetAllItems() ([]models.Item, error) {
	currentItemMap := r.loadMap()
	var items []models.Item
	for _, item := range currentItemMap {
		items = append(items, item)
	}
	return items, nil
}

func (r *BrowserRepository) GetStateHash() (string, error) {
	items, _ := r.GetAllItems()
	return engine.GenerateRootHash(items), nil
}

// Helpers for LocalStorage

func (r *BrowserRepository) loadMap() map[string]models.Item {
	jsonStr := js.Global().Get("localStorage").Call("getItem", r.storeName)
	if jsonStr.IsNull() || jsonStr.IsUndefined() {
		return make(map[string]models.Item)
	}
	
	var itemsFunc map[string]models.Item
	err := json.Unmarshal([]byte(jsonStr.String()), &itemsFunc)
	if err != nil {
		fmt.Println("Error decoding local storage:", err)
		return make(map[string]models.Item)
	}
	return itemsFunc
}

func (r *BrowserRepository) saveMap(items map[string]models.Item) error {
	data, err := json.Marshal(items)
	if err != nil {
		return err
	}
	js.Global().Get("localStorage").Call("setItem", r.storeName, string(data))
	return nil
}
