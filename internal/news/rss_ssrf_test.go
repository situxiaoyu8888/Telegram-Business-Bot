package news

import "testing"

func TestFeedsCannotReachPrivateNetworks(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:80", "10.1.2.3:6379", "169.254.169.254:80", "192.168.1.1:443", "[::1]:80", "0.0.0.0:80", "100.100.100.200:80", "198.18.0.1:80", "[64:ff9b::a00:1]:80", "[::ffff:127.0.0.1]:80"} {
		if publicOnly("tcp", addr, nil) == nil {
			t.Fatalf("%s allowed", addr)
		}
	}
	if err := publicOnly("tcp", "93.184.216.34:443", nil); err != nil {
		t.Fatal("public address refused:", err)
	}
}
