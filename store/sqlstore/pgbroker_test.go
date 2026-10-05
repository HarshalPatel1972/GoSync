package sqlstore

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPostgresBrokerFansOutAcrossInstances(t *testing.T) {
	dsn := os.Getenv("GOSYNC_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set GOSYNC_TEST_POSTGRES to run")
	}
	st, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	type got struct{ ns, origin string }
	var mu sync.Mutex
	received := map[int][]got{}
	brokers := make([]*PostgresBroker, 2)
	for i := range brokers {
		b := NewPostgresBroker(st, dsn, log)
		defer b.Close()
		b.Subscribe(func(ns, origin string) {
			mu.Lock()
			received[i] = append(received[i], got{ns, origin})
			mu.Unlock()
		})
		brokers[i] = b
	}
	time.Sleep(500 * time.Millisecond) // let both LISTEN

	// Instance 0 publishes several changes in one flush window; one namespace
	// is changed by two connections, so its origin must become "".
	brokers[0].Publish(context.Background(), "alice", "conn-1")
	brokers[0].Publish(context.Background(), "bob", "conn-2")
	brokers[0].Publish(context.Background(), "bob", "conn-3")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(received[0]) >= 2 && len(received[1]) >= 2
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range brokers {
		m := map[string]string{}
		for _, g := range received[i] {
			m[g.ns] = g.origin
		}
		if m["alice"] != "conn-1" || m["bob"] != "" || len(m) != 2 {
			t.Fatalf("instance %d received %v", i, received[i])
		}
	}
}
