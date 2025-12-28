package repository

import (
	"errors"
	"sync"

	"github.com/HarshalPatel1972/GoSync/shared/engine"
	"github.com/HarshalPatel1972/GoSync/shared/models"
)

type MemoryRepository struct {
	items map[string]models.Item
	mutex sync.RWMutex
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		items: make(map[string]models.Item),
	}
}

func (r *MemoryRepository) PutItem(item models.Item) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.items[item.ID] = item
	return nil
}

func (r *MemoryRepository) GetItem(id string) (models.Item, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	item, ok := r.items[id]
	if !ok {
		return models.Item{}, errors.New("item not found")
	}
	return item, nil
}

func (r *MemoryRepository) GetAllItems() ([]models.Item, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	var items []models.Item
	for _, item := range r.items {
		items = append(items, item)
	}
	return items, nil
}

func (r *MemoryRepository) GetStateHash() (string, error) {
	items, _ := r.GetAllItems()
	return engine.GenerateRootHash(items), nil
}
