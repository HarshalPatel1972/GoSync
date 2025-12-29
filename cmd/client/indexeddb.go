//go:build js && wasm

package main

import (
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/HarshalPatel1972/GoSync/shared/engine"
	"github.com/HarshalPatel1972/GoSync/shared/models"
)

type BrowserRepository struct{}

func NewBrowserRepository() *BrowserRepository {
	return &BrowserRepository{}
}

func Await(promise js.Value) (js.Value, error) {
	resultCh := make(chan js.Value)
	errCh := make(chan error)

	onSuccess := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		resultCh <- args[0]
		return nil
	})
	onError := js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		errCh <- fmt.Errorf(args[0].String())
		return nil
	})

	promise.Call("then", onSuccess).Call("catch", onError)

	select {
	case res := <-resultCh:
		onSuccess.Release()
		onError.Release()
		return res, nil
	case err := <-errCh:
		onSuccess.Release()
		onError.Release()
		return js.Undefined(), err
	}
}

func (r *BrowserRepository) PutItem(item models.Item) error {
	jsonBytes, err := json.Marshal(item)
	if err != nil {
		return err
	}
	
	promise := js.Global().Get("GoSyncDB").Call("save", item.ID, string(jsonBytes))
	_, err = Await(promise)
	return err
}

func (r *BrowserRepository) GetItem(id string) (models.Item, error) {
	// Not implemented in this phase, simpler to use GetAll
	return models.Item{}, fmt.Errorf("GetItem not implemented for IDB")
}

func (r *BrowserRepository) GetAllItems() ([]models.Item, error) {
	promise := js.Global().Get("GoSyncDB").Call("getAll")
	result, err := Await(promise)
	if err != nil {
		return nil, err
	}

	length := result.Length()
	items := make([]models.Item, length)

	for i := 0; i < length; i++ {
		jsonStr := result.Index(i).String()
		var item models.Item
		err := json.Unmarshal([]byte(jsonStr), &item)
		if err != nil {
			fmt.Println("Error unmarshalling item:", err)
			continue
		}
		items[i] = item
	}

	return items, nil
}

func (r *BrowserRepository) GetStateHash() (string, error) {
	items, err := r.GetAllItems()
	if err != nil {
		return "", err
	}
	return engine.GenerateRootHash(items), nil
}
