package wallet

import (
	"bytes"
	"crypto/sha256"
	"math/big"
	"strings"
)

// ValidTRONAddress validates the Base58Check checksum and mainnet address prefix.
func ValidTRONAddress(address string) bool {
	if len(address) != 34 || address[0] != 'T' {
		return false
	}
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	n := new(big.Int)
	for _, r := range address {
		i := strings.IndexRune(alphabet, r)
		if i < 0 {
			return false
		}
		n.Mul(n, big.NewInt(58))
		n.Add(n, big.NewInt(int64(i)))
	}
	raw := n.Bytes()
	if len(raw) != 25 || raw[0] != 0x41 {
		return false
	}
	first := sha256.Sum256(raw[:21])
	second := sha256.Sum256(first[:])
	return bytes.Equal(raw[21:], second[:4])
}
