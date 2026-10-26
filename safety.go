package main

import (
	"fmt"
	"io"
)

const (
	maxRPCBytes      = 128 << 20
	maxRequestBytes  = 32 << 20
	maxEventBytes    = 8 << 20
	maxResponseBytes = 64 << 20
	maxCatalogBytes  = 2 << 20
)

// limitedBody detects oversize responses rather than silently truncating them.
type limitedBody struct {
	io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, &zenStreamError{message: "Upstream response exceeds size limit"}
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return 0, &zenStreamError{message: "Upstream response exceeds size limit"}
	}
	return n, err
}

func readAllBounded(body io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("upstream body exceeds size limit")
	}
	return data, nil
}
