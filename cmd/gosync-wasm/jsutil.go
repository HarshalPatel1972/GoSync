//go:build js && wasm

package main

import (
	"context"
	"errors"
	"syscall/js"
)

// await blocks the calling goroutine until a JS promise settles. It must not
// be called from a JS callback's own goroutine.
func await(ctx context.Context, p js.Value) (js.Value, error) {
	type result struct {
		v   js.Value
		err error
	}
	ch := make(chan result, 1)
	onOK := js.FuncOf(func(_ js.Value, args []js.Value) any {
		v := js.Undefined()
		if len(args) > 0 {
			v = args[0]
		}
		ch <- result{v: v}
		return nil
	})
	onErr := js.FuncOf(func(_ js.Value, args []js.Value) any {
		msg := "promise rejected"
		if len(args) > 0 {
			if m := args[0].Get("message"); m.Type() == js.TypeString {
				msg = m.String()
			} else {
				msg = js.Global().Get("String").Invoke(args[0]).String()
			}
		}
		ch <- result{err: errors.New(msg)}
		return nil
	})
	defer onOK.Release()
	defer onErr.Release()
	p.Call("then", onOK, onErr)
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return js.Undefined(), ctx.Err()
	}
}

// promise runs fn on a new goroutine and returns a JS Promise for its result.
func promise(fn func() (any, error)) js.Value {
	var executor js.Func
	executor = js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve, reject := args[0], args[1]
		go func() {
			v, err := fn()
			if err != nil {
				reject.Invoke(js.Global().Get("Error").New(err.Error()))
				return
			}
			resolve.Invoke(v)
		}()
		return nil
	})
	defer executor.Release() // the executor runs synchronously inside New
	return js.Global().Get("Promise").New(executor)
}

func jsError(err error) js.Value {
	return js.Global().Get("Error").New(err.Error())
}
