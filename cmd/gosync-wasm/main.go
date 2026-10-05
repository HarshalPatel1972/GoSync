//go:build js && wasm

// Command gosync-wasm is the GoSync browser engine. Load it through
// sdk/js/gosync.js, which supplies the IndexedDB bridge and multi-tab
// coordination and exposes the public JavaScript API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"syscall/js"

	"github.com/HarshalPatel1972/GoSync/client"
)

func main() {
	js.Global().Set("__gosyncCreate", js.FuncOf(create))
	if ready := js.Global().Get("__gosyncReady"); ready.Type() == js.TypeFunction {
		ready.Invoke()
	}
	select {} // keep the runtime alive
}

// create(options) -> Promise<engine>. options: {url, getToken, bridge,
// nodeSuffix, debug}.
func create(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return js.Global().Get("Promise").Call("reject", jsError(errors.New("missing options")))
	}
	opts := args[0]
	return promise(func() (any, error) {
		level := slog.LevelWarn
		if opts.Get("debug").Truthy() {
			level = slog.LevelDebug
		}
		getToken := opts.Get("getToken")
		c, err := client.New(context.Background(), client.Options{
			URL:        opts.Get("url").String(),
			Store:      idbStore{bridge: opts.Get("bridge")},
			Dialer:     browserDialer{},
			NodeSuffix: opts.Get("nodeSuffix").String(),
			Logger:     slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})),
			Token: func(ctx context.Context) (string, error) {
				if getToken.Type() != js.TypeFunction {
					return "", nil
				}
				v, err := await(ctx, js.Global().Get("Promise").Call("resolve", getToken.Invoke()))
				if err != nil {
					return "", err
				}
				return v.String(), nil
			},
		})
		if err != nil {
			return nil, err
		}
		return engineObject(c), nil
	})
}

// engineObject exposes a client to JavaScript. Field values cross as JSON
// strings; gosync.js converts them.
func engineObject(c *client.Client) js.Value {
	obj := js.Global().Get("Object").New()
	method := func(name string, fn func(args []js.Value) any) {
		obj.Set(name, js.FuncOf(func(_ js.Value, args []js.Value) any { return fn(args) }))
	}
	ctx := context.Background()

	// set(collection, id, fieldsJSON) -> Promise<void>
	method("set", func(a []js.Value) any {
		coll, id, fieldsJSON := a[0].String(), a[1].String(), a[2].String()
		return promise(func() (any, error) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
				return nil, errors.New("fields must be a JSON object")
			}
			return nil, c.Set(ctx, coll, id, fields)
		})
	})
	// delete(collection, id) -> Promise<void>
	method("delete", func(a []js.Value) any {
		coll, id := a[0].String(), a[1].String()
		return promise(func() (any, error) { return nil, c.Delete(ctx, coll, id) })
	})
	// get(collection, id) -> Promise<string|null>  (JSON {id, fields})
	method("get", func(a []js.Value) any {
		coll, id := a[0].String(), a[1].String()
		return promise(func() (any, error) {
			d, ok, err := c.Get(ctx, coll, id)
			if err != nil || !ok {
				return js.Null(), err
			}
			b, err := json.Marshal(docJSON(d))
			return string(b), err
		})
	})
	// list(collection) -> Promise<string>  (JSON [{id, fields}])
	method("list", func(a []js.Value) any {
		coll := a[0].String()
		return promise(func() (any, error) {
			docs, err := c.List(ctx, coll)
			if err != nil {
				return nil, err
			}
			out := make([]any, len(docs))
			for i, d := range docs {
				out[i] = docJSON(d)
			}
			b, err := json.Marshal(out)
			return string(b), err
		})
	})
	// subscribe(cb) -> unsubscribe. cb receives {kind, collection, ids, status, error}.
	method("subscribe", func(a []js.Value) any {
		cb := a[0]
		unsub := c.Subscribe(func(ev client.Event) {
			e := map[string]any{"kind": ev.Kind}
			switch ev.Kind {
			case "change":
				ids := make([]any, len(ev.IDs))
				for i, id := range ev.IDs {
					ids[i] = id
				}
				e["collection"], e["ids"] = ev.Collection, ids
			case "status":
				e["status"] = string(ev.Status)
			case "error":
				e["error"] = ev.Err.Error()
			}
			cb.Invoke(js.ValueOf(e))
		})
		var f js.Func
		f = js.FuncOf(func(js.Value, []js.Value) any {
			unsub()
			f.Release()
			return nil
		})
		return f
	})
	// run() -> stop function. Starts the sync loop (leader tab only).
	method("run", func([]js.Value) any {
		runCtx, cancel := context.WithCancel(ctx)
		go c.Run(runCtx)
		var f js.Func
		f = js.FuncOf(func(js.Value, []js.Value) any {
			cancel()
			f.Release()
			return nil
		})
		return f
	})
	method("status", func([]js.Value) any { return string(c.Status()) })
	method("clientId", func([]js.Value) any { return c.ClientID() })
	method("notifyOutbox", func([]js.Value) any { c.NotifyOutbox(); return nil })
	method("reconnect", func([]js.Value) any { c.Reconnect(); return nil })
	method("observeHlc", func(a []js.Value) any { c.ObserveHLC(a[0].String()); return nil })
	return obj
}

func docJSON(d client.Document) map[string]any {
	return map[string]any{"id": d.ID, "fields": d.Fields}
}
