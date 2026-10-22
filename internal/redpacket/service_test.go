package redpacket

import "testing"

func TestSplitLuckyIsPositiveAndExact(t *testing.T) {
	for _, c := range []struct {
		units int64
		n     int
	}{{1, 1}, {10, 10}, {11, 10}, {100, 3}, {1000000, 100}, {5, 2}} {
		for round := 0; round < 200; round++ {
			shares, err := splitLucky(c.units, c.n)
			if err != nil || len(shares) != c.n {
				t.Fatalf("%+v: %v %v", c, shares, err)
			}
			var sum int64
			for _, s := range shares {
				if s < 1 {
					t.Fatalf("%+v: non-positive share in %v", c, shares)
				}
				sum += s
			}
			if sum != c.units {
				t.Fatalf("%+v: shares sum to %d", c, sum)
			}
		}
	}
}

func TestParseTarget(t *testing.T) {
	if u, id, err := ParseTarget("@Alice_01"); err != nil || u != "Alice_01" || id != 0 {
		t.Fatal(u, id, err)
	}
	if u, id, err := ParseTarget("123456"); err != nil || u != "" || id != 123456 {
		t.Fatal(u, id, err)
	}
	for _, bad := range []string{"", "@ab", "hello world", "-5"} {
		if _, _, err := ParseTarget(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
