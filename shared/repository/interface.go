package repository

import "github.com/HarshalPatel1972/GoSync/shared/models"

type Repository interface {
	PutItem(item models.Item) error
	GetItem(id string) (models.Item, error)
	GetAllItems() ([]models.Item, error)
	GetStateHash() (string, error)
}
