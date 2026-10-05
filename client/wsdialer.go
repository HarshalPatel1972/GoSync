//go:build !js

package client

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocketDialer dials with gorilla/websocket. It is the Dialer for native
// (non-browser) Go programs.
type WebSocketDialer struct {
	// Header is sent with the handshake (e.g. a custom User-Agent).
	Header http.Header
}

func (d WebSocketDialer) Dial(ctx context.Context, url string) (Conn, error) {
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, url, d.Header)
	if err != nil {
		return nil, err
	}
	return &wsConn{ws: ws}, nil
}

type wsConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	once    sync.Once
}

func (c *wsConn) Read(ctx context.Context) ([]byte, error) {
	// gorilla reads are not context-aware; close the socket on cancel.
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_, b, err := c.ws.ReadMessage()
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return b, err
}

func (c *wsConn) Write(ctx context.Context, msg []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		c.ws.SetWriteDeadline(dl)
	} else {
		c.ws.SetWriteDeadline(time.Time{})
	}
	return c.ws.WriteMessage(websocket.TextMessage, msg)
}

func (c *wsConn) Close() error {
	var err error
	c.once.Do(func() { err = c.ws.Close() })
	return err
}
