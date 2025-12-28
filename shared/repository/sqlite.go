package repository

import (
	"github.com/HarshalPatel1972/GoSync/shared/engine"
	"github.com/HarshalPatel1972/GoSync/shared/models"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type SQLiteRepository struct {
	db *gorm.DB
}

func NewSQLiteRepository(filePath string) (*SQLiteRepository, error) {
	db, err := gorm.Open(sqlite.Open(filePath), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	err = db.AutoMigrate(&models.Item{})
	if err != nil {
		return nil, err
	}

	return &SQLiteRepository{db: db}, nil
}

func (r *SQLiteRepository) PutItem(item models.Item) error {
	return r.db.Save(&item).Error
}

func (r *SQLiteRepository) GetItem(id string) (models.Item, error) {
	var item models.Item
	err := r.db.First(&item, "id = ?", id).Error
	return item, err
}

func (r *SQLiteRepository) GetAllItems() ([]models.Item, error) {
	var items []models.Item
	err := r.db.Find(&items).Error
	return items, err
}

func (r *SQLiteRepository) GetStateHash() (string, error) {
	items, err := r.GetAllItems()
	if err != nil {
		return "", err
	}
	return engine.GenerateRootHash(items), nil
}
