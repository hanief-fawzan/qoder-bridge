// encoding.go — WAF-bypass body encoding for Qoder API.
//
// Algorithm:
//  1. base64-encode the plaintext bytes (standard alphabet)
//  2. Rearrange: split into thirds, reorder as [tail][mid][head]
//  3. Substitute each character via a custom alphabet mapping
//
// The encoded body is sent with &Encode=1 in the URL so the server decodes in reverse.
package main

import "encoding/base64"

const (
	qoderStdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	qoderCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
)

var stdToCustom [128]byte

func init() {
	for i := 0; i < 64; i++ {
		stdToCustom[qoderStdAlphabet[i]] = qoderCustomAlphabet[i]
	}
	stdToCustom['='] = '$'
}

// qoderEncodeBody encodes plaintext using Qoder's WAF-bypass scheme.
func qoderEncodeBody(plaintext []byte) []byte {
	std := base64.StdEncoding.EncodeToString(plaintext)
	n := len(std)
	a := n / 3

	// Rearrange: [tail][mid][head]
	rearranged := make([]byte, 0, n)
	rearranged = append(rearranged, std[n-a:]...)  // tail
	rearranged = append(rearranged, std[a:n-a]...)  // mid
	rearranged = append(rearranged, std[:a]...)      // head

	out := make([]byte, n)
	for i := 0; i < n; i++ {
		c := rearranged[i]
		if c < 128 && stdToCustom[c] != 0 {
			out[i] = stdToCustom[c]
		} else {
			out[i] = c
		}
	}
	return out
}
