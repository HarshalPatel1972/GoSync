//go:build js && wasm

package main

import (
	"context"
	"errors"
	"sync"
	"syscall/js"

	"github.com/HarshalPatel1972/GoSync/client"
)

// browserDialer opens connections with the browser's WebSocket API.
type browserDialer struct{}

type browserConn struct {
	ws     js.Value
	funcs  []js.Func
	opened chan struct{}
	closed chan struct{}

	mu       sync.Mutex
	queue    [][]byte
	ready    chan struct{} // capacity 1: queue became non-empty
	closeErr error
	once     sync.Once
}

func (browserDialer) Dial(ctx context.Context, url string) (client.Conn, error) {
	c := &browserConn{
		opened: make(chan struct{}),
		closed: make(chan struct{}),
		ready:  make(chan struct{}, 1),
	}
	c.ws = js.Global().Get("WebSocket").New(url)
	on := func(event string, fn func(js.Value)) {
		f := js.FuncOf(func(_ js.Value, args []js.Value) any {
			fn(args[0])
			return nil
		})
		c.funcs = append(c.funcs, f)
		c.ws.Set(event, f)
	}
	on("onopen", func(js.Value) { close(c.opened) })
	on("onmessage", func(ev js.Value) {
		// JS callbacks must not block, so queue without bound; the sync
		// loop has at most a handful of requests in flight.
		c.mu.Lock()
		c.queue = append(c.queue, []byte(ev.Get("data").String()))
		c.mu.Unlock()
		select {
		case c.ready <- struct{}{}:
		default:
		}
	})
	on("onclose", func(ev js.Value) {
		c.mu.Lock()
		if c.closeErr == nil {
			c.closeErr = errors.New("websocket closed: " + ev.Get("reason").String())
		}
		c.mu.Unlock()
		c.markClosed()
	})
	// onerror carries no detail in browsers; onclose follows it.
	on("onerror", func(js.Value) {})

	select {
	case <-c.opened:
		return c, nil
	case <-c.closed:
		c.Close()
		return nil, errors.New("websocket connection failed")
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	}
}

func (c *browserConn) markClosed() {
	c.once.Do(func() { close(c.closed) })
}

func (c *browserConn) Read(ctx context.Context) ([]byte, error) {
	for {
		c.mu.Lock()
		if len(c.queue) > 0 {
			msg := c.queue[0]
			c.queue = c.queue[1:]
			c.mu.Unlock()
			return msg, nil
		}
		c.mu.Unlock()
		select {
		case <-c.ready:
		case <-c.closed:
			// Deliver anything that arrived before the close.
			c.mu.Lock()
			pending := len(c.queue)
			err := c.closeErr
			c.mu.Unlock()
			if pending == 0 {
				return nil, err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *browserConn) Write(_ context.Context, msg []byte) error {
	select {
	case <-c.closed:
		return errors.New("websocket closed")
	default:
	}
	if c.ws.Get("readyState").Int() != 1 { // OPEN
		return errors.New("websocket not open")
	}
	c.ws.Call("send", string(msg))
	return nil
}

func (c *browserConn) Close() error {
	c.ws.Call("close")
	c.markClosed()
	for _, event := range []string{"onopen", "onmessage", "onclose", "onerror"} {
		c.ws.Set(event, js.Null())
	}
	for _, f := range c.funcs {
		f.Release()
	}
	c.funcs = nil
	return nil
}
