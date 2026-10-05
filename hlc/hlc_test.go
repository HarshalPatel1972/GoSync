package hlc

import "testing"

func TestEncodingPreservesOrder(t *testing.T) {
	ts := []Timestamp{
		{Wall: 5, Counter: 0, Node: "a"},
		{Wall: 5, Counter: 0, Node: "b"},
		{Wall: 5, Counter: 1, Node: "a"},
		{Wall: 6, Counter: 0, Node: "a"},
		{Wall: 1_700_000_000_000, Counter: 0, Node: "a"},
	}
	for i := 1; i < len(ts); i++ {
		a, b := ts[i-1], ts[i]
		if a.Compare(b) >= 0 {
			t.Fatalf("%v should sort before %v", a, b)
		}
		if a.String() >= b.String() {
			t.Fatalf("encoded %q should sort before %q", a, b)
		}
	}
}

func TestParseRoundTrip(t *testing.T) {
	in := Timestamp{Wall: 1_700_000_000_123, Counter: 42, Node: "client-1"}
	out, err := Parse(in.String())
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("got %+v want %+v", out, in)
	}
	for _, bad := range []string{"", "abc", "000000000000001-000000-", "000000000000001x000000-n", "000000000000001-000000-has space"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestClockMonotonicWhenWallStalls(t *testing.T) {
	wall := int64(100)
	c, _ := NewClock("n", func() int64 { return wall })
	a := c.Now()
	b := c.Now()
	wall = 50 // clock goes backwards
	d := c.Now()
	if !(a.Compare(b) < 0 && b.Compare(d) < 0) {
		t.Fatalf("not monotonic: %v %v %v", a, b, d)
	}
}

func TestClockObserveOrdersAfterRemote(t *testing.T) {
	c, _ := NewClock("local", func() int64 { return 100 })
	remote := Timestamp{Wall: 500, Counter: 3, Node: "zzz"}
	c.Observe(remote)
	if got := c.Now(); got.Compare(remote) <= 0 {
		t.Fatalf("%v should be after observed %v", got, remote)
	}
}

func TestClockCounterNeverOverflowsEncoding(t *testing.T) {
	c, _ := NewClock("n", func() int64 { return 100 }) // wall clock stuck
	c.Observe(Timestamp{Wall: 100, Counter: MaxCounter - 1, Node: "r"})
	prev := c.Now()
	for range 5 {
		next := c.Now()
		if next.Counter > MaxCounter || next.String() <= prev.String() {
			t.Fatalf("encoding order broken: %s after %s", next, prev)
		}
		if _, err := Parse(next.String()); err != nil {
			t.Fatal(err)
		}
		prev = next
	}
}
