// cosy.go — COSY signing (RSA-2048 + AES-128-CBC + MD5)
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
	"time"
)

const qoderRSAPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// COSY header constants — must match qodercli values exactly.
const (
	cosyVersion   = "1.0.0"
	clientType    = "5"
	dataPolicy    = "disagree"
	loginVersion  = "v2"
	machineOS     = "x86_64_windows"
	machineType   = "5"
)

var rsaPub *rsa.PublicKey

func init() {
	block, _ := pem.Decode([]byte(qoderRSAPublicKeyPEM))
	if block == nil {
		panic("qoder: invalid RSA public key PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		panic("qoder: parse RSA public key: " + err.Error())
	}
	rsaPub = pub.(*rsa.PublicKey)
}

// pkcs7Pad adds PKCS#7 padding to data for the given block size.
func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - (len(data) % blockSize)
	if pad == 0 {
		pad = blockSize
	}
	return append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

// aesEncryptCBCBase64 encrypts plaintext with AES-128-CBC using key as both
// key and IV (matching qodercli convention), returns base64.
func aesEncryptCBCBase64(plaintext, key []byte) (string, error) {
	if len(key) != 16 {
		return "", fmt.Errorf("aes key must be 16 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	iv := key[:16]
	mode := cipher.NewCBCEncrypter(block, iv)
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	out := make([]byte, len(padded))
	mode.CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

// rsaEncryptBase64 encrypts data with RSA PKCS1v15, returns base64.
func rsaEncryptBase64(data []byte) (string, error) {
	enc, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, data)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// generateAesKey returns first 16 chars of a UUID (matching qodercli convention).
func generateAesKey() string {
	return uuidString()[:16]
}

// uuidString generates a UUID v4 string using crypto/rand.
func uuidString() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// computeSigPath extracts the path after /algo from a URL.
func computeSigPath(requestURL string) string {
	idx := strings.Index(requestURL, "/algo")
	if idx < 0 {
		return ""
	}
	path := requestURL[idx+len("/algo"):]
	if q := strings.Index(path, "?"); q >= 0 {
		path = path[:q]
	}
	return path
}

// CosyCreds holds credentials for signing.
type CosyCreds struct {
	UserID    string
	AuthToken string // job token (jt-...)
	Name      string
	Email     string
	MachineID string
}

// CosyHeaders holds the full set of COSY headers for a request.
type CosyHeaders struct {
	Authorization string
	Headers       map[string]string
}

// BuildCosyHeaders builds the full COSY header set for a Qoder request.
func BuildCosyHeaders(body []byte, requestURL string, creds CosyCreds) (*CosyHeaders, error) {
	if creds.UserID == "" {
		return nil, fmt.Errorf("cosy: userId is empty")
	}
	if creds.AuthToken == "" {
		return nil, fmt.Errorf("cosy: authToken is empty")
	}

	aesKey := []byte(generateAesKey())

	userInfo, _ := json.Marshal(map[string]string{
		"uid":                  creds.UserID,
		"security_oauth_token": creds.AuthToken,
		"name":                 creds.Name,
		"aid":                  "",
		"email":                creds.Email,
	})

	infoB64, err := aesEncryptCBCBase64(userInfo, aesKey)
	if err != nil {
		return nil, fmt.Errorf("cosy: aes encrypt: %w", err)
	}

	cosyKeyB64, err := rsaEncryptBase64(aesKey)
	if err != nil {
		return nil, fmt.Errorf("cosy: rsa encrypt: %w", err)
	}

	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	requestID := uuidString()

	payloadJSON, _ := json.Marshal(map[string]string{
		"version":     "v1",
		"requestId":   requestID,
		"info":        infoB64,
		"cosyVersion": cosyVersion,
		"ideVersion":  "",
	})
	payloadB64 := base64.StdEncoding.EncodeToString(payloadJSON)

	sigPath := computeSigPath(requestURL)
	sigInput := fmt.Sprintf("%s\n%s\n%s\n%s\n%s",
		payloadB64, cosyKeyB64, timestamp, string(body), sigPath)
	sig := md5Hex([]byte(sigInput))

	machineID := creds.MachineID
	if machineID == "" {
		machineID = uuidString()
	}
	bodyHash := md5Hex(body)
	bodyLength := fmt.Sprintf("%d", len(body))

	return &CosyHeaders{
		Authorization: "Bearer COSY." + payloadB64 + "." + sig,
		Headers: map[string]string{
			"Cosy-Key":             cosyKeyB64,
			"Cosy-User":            creds.UserID,
			"Cosy-Date":            timestamp,
			"Cosy-Version":         cosyVersion,
			"Cosy-Machineid":       machineID,
			"Cosy-Machinetoken":    machineID,
			"Cosy-Machinetype":     machineType,
			"Cosy-Machineos":       machineOS,
			"Cosy-Clienttype":      clientType,
			"Cosy-Clientip":        "127.0.0.1",
			"Cosy-Bodyhash":        bodyHash,
			"Cosy-Bodylength":      bodyLength,
			"Cosy-Sigpath":         sigPath,
			"Cosy-Data-Policy":     dataPolicy,
			"Cosy-Organization-Id": "",
			"Cosy-Organization-Tags": "",
			"Login-Version":        loginVersion,
			"X-Request-Id":         uuidString(),
		},
	}, nil
}

func md5Hex(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])
}
