// Package qrcode renders QR codes for deposit addresses.
package qrcode

import goqr "github.com/skip2/go-qrcode"

// PNG renders text as a 512×512 PNG QR code with medium error correction.
func PNG(text string) ([]byte, error) { return goqr.Encode(text, goqr.Medium, 512) }
