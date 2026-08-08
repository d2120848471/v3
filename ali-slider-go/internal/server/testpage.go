package server

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"net/http"
)

const testPageNoncePlaceholder = "__ALI_SLIDER_CSP_NONCE__"

//go:embed web/test.html
var testPageHTML []byte

var readTestPageNonce = rand.Read

func writeTestPage(w http.ResponseWriter) int {
	nonce, err := newTestPageNonce()
	if err != nil {
		setPrivateResponseHeaders(w.Header())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("API 测试页暂时不可用"))
		return http.StatusInternalServerError
	}

	body := bytes.ReplaceAll(testPageHTML, []byte(testPageNoncePlaceholder), []byte(nonce))
	header := w.Header()
	setPrivateResponseHeaders(header)
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	header.Set("Content-Security-Policy", fmt.Sprintf(
		"default-src 'none'; connect-src 'self'; script-src 'nonce-%s'; script-src-attr 'none'; style-src 'nonce-%s'; style-src-attr 'none'; img-src 'none'; font-src 'none'; media-src 'none'; object-src 'none'; worker-src 'none'; manifest-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
		nonce,
		nonce,
	))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return http.StatusOK
}

func newTestPageNonce() (string, error) {
	buffer := make([]byte, 16)
	if _, err := readTestPageNonce(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
