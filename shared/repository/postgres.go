package repository

import (
	"github.com/HarshalPatel1972/GoSync/shared/engine"
	"github.com/HarshalPatel1972/GoSync/shared/models"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type PostgresRepository struct {
	db *gorm.DB
}

func NewPostgresRepository(dsn string) (*PostgresRepository, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	err = db.AutoMigrate(&models.Item{})
	if err != nil {
		return nil, err
	}

	return &PostgresRepository{db: db}, nil
}

func (r *PostgresRepository) PutItem(item models.Item) error {
	return r.db.Save(&item).Error
}

func (r *PostgresRepository) GetItem(id string) (models.Item, error) {
	var item models.Item
	err := r.db.First(&item, "id = ?", id).Error
	return item, err
}

func (r *PostgresRepository) GetAllItems() ([]models.Item, error) {
	var items []models.Item
	err := r.db.Find(&items).Error
	return items, err
}

func (r *PostgresRepository) GetStateHash() (string, error) {
	items, err := r.GetAllItems()
	if err != nil {
		return "", err
	}
	return engine.GenerateRootHash(items), nil
}
