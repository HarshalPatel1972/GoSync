package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/HarshalPatel1972/GoSync/hlc"
	"github.com/HarshalPatel1972/GoSync/protocol"
)

const writeTimeout = 10 * time.Second

// conn is one authenticated client session.
type conn struct {
	s         *Server
	ws        *websocket.Conn
	id        string
	log       *slog.Logger
	principal Principal
	clientID  string
	limiter   *rate.Limiter

	out   chan []byte   // frames for the writer goroutine
	pokes chan struct{} // capacity 1: coalesced change notifications
	done  chan struct{} // closed once the socket is closed

	// finish asks the writer to flush queued frames, then close with
	// finishCode. This guarantees a fatal error frame reaches the client.
	finishOnce   sync.Once
	finishCode   int
	finishReason string
	drain        chan struct{}
	writerDone   chan struct{}

	closeOnce sync.Once
}

// errClosed signals that the session ended and the error is already reported.
var errClosed = errors.New("connection closed")

func newConn(s *Server, ws *websocket.Conn, remote string) *conn {
	id := s.instance + "-" + randomID(8)
	return &conn{
		s:          s,
		ws:         ws,
		id:         id,
		log:        s.log.With("conn", id, "remote", remote),
		limiter:    rate.NewLimiter(rate.Limit(s.cfg.MessagesPerSec), s.cfg.MessageBurst),
		out:        make(chan []byte, 32),
		pokes:      make(chan struct{}, 1),
		done:       make(chan struct{}),
		drain:      make(chan struct{}),
		writerDone: make(chan struct{}),
	}
}

// finish flushes pending frames, then closes the session.
func (c *conn) finish(code int, reason string) {
	c.finishOnce.Do(func() {
		c.finishCode, c.finishReason = code, reason
		close(c.drain)
	})
}

// closeWith sends a close frame (best effort) and tears the session down.
func (c *conn) closeWith(code int, reason string) {
	c.closeOnce.Do(func() {
		msg := websocket.FormatCloseMessage(code, reason)
		c.ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
		close(c.done)
		c.ws.Close()
	})
}

func (c *conn) serve(ctx context.Context) {
	c.ws.SetReadLimit(protocol.MaxMessageBytes)
	defer c.closeWith(websocket.CloseNormalClosure, "")

	if err := c.handshake(ctx); err != nil {
		if !errors.Is(err, errClosed) {
			c.log.Info("handshake failed", "err", err)
		}
		return
	}
	c.s.hub.add(c)
	defer c.s.hub.remove(c)
	c.log.Info("session started", "sub", c.principal.Subject, "client", c.clientID)

	go c.writeLoop()
	defer func() {
		c.finish(websocket.CloseNormalClosure, "")
		select {
		case <-c.writerDone:
		case <-time.After(2 * time.Second):
		}
	}()
	if !c.principal.ExpiresAt.IsZero() {
		t := time.AfterFunc(time.Until(c.principal.ExpiresAt), func() {
			c.sendError(protocol.ErrUnauthorized, "token expired", true)
		})
		defer t.Stop()
	}

	err := c.readLoop(ctx)
	var ce *websocket.CloseError
	if err != nil && !errors.Is(err, errClosed) && !errors.As(err, &ce) {
		c.log.Info("session ended", "err", err)
	}
}

// handshake reads hello, authenticates, and answers with welcome. It writes
// directly because the writer goroutine is not running yet.
func (c *conn) handshake(ctx context.Context) error {
	c.ws.SetReadDeadline(time.Now().Add(c.s.cfg.HelloTimeout))
	var env protocol.Envelope
	if err := c.ws.ReadJSON(&env); err != nil {
		return err
	}
	fail := func(code, msg string) error {
		b, _ := protocol.Encode(protocol.TypeError, protocol.Error{Code: code, Message: msg, Fatal: true})
		c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
		c.ws.WriteMessage(websocket.TextMessage, b)
		c.closeWith(websocket.ClosePolicyViolation, code)
		return fmt.Errorf("%s: %s", code, msg)
	}
	if env.Type != protocol.TypeHello {
		return fail(protocol.ErrBadRequest, "first message must be hello")
	}
	var hello protocol.Hello
	if err := json.Unmarshal(env.Data, &hello); err != nil {
		return fail(protocol.ErrBadRequest, "malformed hello")
	}
	if hello.Version != protocol.Version {
		return fail(protocol.ErrVersion, fmt.Sprintf("server speaks protocol version %d", protocol.Version))
	}
	if err := protocol.ValidateClientID(hello.ClientID); err != nil {
		return fail(protocol.ErrBadRequest, err.Error())
	}
	if len(hello.Token) > protocol.MaxTokenLen {
		return fail(protocol.ErrUnauthorized, "token too long")
	}
	authCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	p, err := c.s.cfg.Auth.Authenticate(authCtx, hello.Token)
	cancel()
	if err != nil {
		c.log.Info("authentication failed", "err", err)
		return fail(protocol.ErrUnauthorized, "invalid or expired token")
	}
	c.principal, c.clientID = p, hello.ClientID

	last, err := c.s.cfg.Store.LastMutationID(ctx, p.Namespace, hello.ClientID)
	if err != nil {
		c.log.Error("load client state", "err", err)
		return fail(protocol.ErrInternal, "internal error")
	}
	b, _ := protocol.Encode(protocol.TypeWelcome, protocol.Welcome{ServerTime: time.Now().UnixMilli(), LastMutationID: last})
	c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

func (c *conn) readLoop(ctx context.Context) error {
	pongWait := 2*c.s.cfg.PingInterval + 10*time.Second
	c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		var env protocol.Envelope
		if err := c.ws.ReadJSON(&env); err != nil {
			return err
		}
		c.ws.SetReadDeadline(time.Now().Add(pongWait))
		if !c.limiter.Allow() {
			c.sendError(protocol.ErrRateLimited, "too many messages", true)
			return errClosed
		}
		var err error
		switch env.Type {
		case protocol.TypePush:
			err = c.handlePush(ctx, env.Data)
		case protocol.TypePull:
			err = c.handlePull(ctx, env.Data)
		default:
			err = badRequest("unknown message type %q", env.Type)
		}
		var br badRequestError
		switch {
		case err == nil:
		case errors.As(err, &br):
			// Protocol violations mean a broken client; disconnect it.
			c.sendError(protocol.ErrBadRequest, br.Error(), true)
			return errClosed
		case errors.Is(err, errClosed):
			return err
		default:
			c.log.Error("request failed", "type", env.Type, "err", err)
			c.sendError(protocol.ErrInternal, "internal error", false)
		}
	}
}

type badRequestError struct{ msg string }

func (e badRequestError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return badRequestError{fmt.Sprintf(format, args...)}
}

func (c *conn) handlePush(ctx context.Context, data json.RawMessage) error {
	var push protocol.Push
	if err := json.Unmarshal(data, &push); err != nil {
		return badRequest("malformed push")
	}
	if len(push.Mutations) > protocol.MaxMutationsPerPush {
		return badRequest("at most %d mutations per push", protocol.MaxMutationsPerPush)
	}
	maxWall := time.Now().Add(c.s.cfg.MaxClockSkew).UnixMilli()
	valid := make([]protocol.Mutation, 0, len(push.Mutations))
	var rejected []protocol.Rejection
	var upTo int64
	for i, m := range push.Mutations {
		if i > 0 && m.ID <= push.Mutations[i-1].ID {
			return badRequest("mutation ids must be strictly increasing")
		}
		upTo = m.ID
		reject := func(reason string) {
			rejected = append(rejected, protocol.Rejection{ID: m.ID, Reason: reason})
		}
		if err := protocol.ValidateMutation(m); err != nil {
			if m.ID <= 0 {
				return badRequest("mutation id must be positive")
			}
			reject(err.Error())
			continue
		}
		if ts, _ := hlc.Parse(m.HLC); ts.Wall > maxWall {
			reject("timestamp is too far in the future; check the device clock")
			continue
		}
		valid = append(valid, m)
	}

	out, err := c.s.cfg.Store.Push(ctx, c.principal.Namespace, c.clientID, valid, upTo)
	if err != nil {
		return err
	}
	if out.Changed {
		if err := c.s.cfg.Broker.Publish(ctx, c.principal.Namespace, c.id); err != nil {
			c.log.Warn("publish change notification", "err", err)
		}
	}
	if len(rejected) > 0 {
		c.log.Warn("rejected mutations", "count", len(rejected), "first", rejected[0].Reason)
	}
	return c.send(protocol.TypePushResult, protocol.PushResult{LastMutationID: out.LastMutationID, Rejected: rejected})
}

func (c *conn) handlePull(ctx context.Context, data json.RawMessage) error {
	var pull protocol.Pull
	if err := json.Unmarshal(data, &pull); err != nil {
		return badRequest("malformed pull")
	}
	res, err := c.s.cfg.Store.Pull(ctx, c.principal.Namespace, pull.Cursor, c.s.cfg.PullBudgetBytes)
	if err != nil {
		return err
	}
	return c.send(protocol.TypePullResult, res)
}

func (c *conn) send(typ string, payload any) error {
	b, err := protocol.Encode(typ, payload)
	if err != nil {
		return err
	}
	select {
	case c.out <- b:
		return nil
	case <-c.done:
		return errClosed
	}
}

func (c *conn) sendError(code, msg string, fatal bool) {
	c.send(protocol.TypeError, protocol.Error{Code: code, Message: msg, Fatal: fatal})
	if fatal {
		c.finish(websocket.ClosePolicyViolation, code)
	}
}

// writeLoop is the only goroutine writing data frames after the handshake.
func (c *conn) writeLoop() {
	defer close(c.writerDone)
	ping := time.NewTicker(c.s.cfg.PingInterval)
	defer ping.Stop()
	poke, _ := protocol.Encode(protocol.TypePoke, nil)
	write := func(b []byte) bool {
		c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := c.ws.WriteMessage(websocket.TextMessage, b); err != nil {
			c.closeWith(websocket.CloseAbnormalClosure, "")
			return false
		}
		return true
	}
	for {
		select {
		case <-c.done:
			return
		case <-c.drain:
			for {
				select {
				case b := <-c.out:
					if !write(b) {
						return
					}
				default:
					c.closeWith(c.finishCode, c.finishReason)
					return
				}
			}
		case b := <-c.out:
			if !write(b) {
				return
			}
		case <-c.pokes:
			if !write(poke) {
				return
			}
		case <-ping.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout)); err != nil {
				c.closeWith(websocket.CloseAbnormalClosure, "")
				return
			}
		}
	}
}
