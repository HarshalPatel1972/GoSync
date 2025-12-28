package main

import (
	"fmt"
	"syscall/js"
)

func main() {
	fmt.Println("GoSync WASM initialized")

	js.Global().Set("addItemToStore", js.FuncOf(addItemToStore))

	select {} // Keep the Go program running
}

func addItemToStore(this js.Value, args []js.Value) interface{} {
	if len(args) < 1 {
		fmt.Println("No content provided")
		return nil
	}
	content := args[0].String()
	fmt.Printf("Adding item: %s\n", content)
	return nil
}
