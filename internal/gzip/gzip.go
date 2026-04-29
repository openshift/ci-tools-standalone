package gzip

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
)

// ReadFileMaybeGZIP returns the decompressed contents if the file is gzipped,
// or otherwise the raw contents.
func ReadFileMaybeGZIP(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ReadBytesMaybeGZIP(b)
}

// ReadBytesMaybeGZIP returns the decompressed data if it has a gzip header,
// or otherwise returns the input unchanged.
func ReadBytesMaybeGZIP(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(data, []byte("\x1F\x8B")) {
		return data, nil
	}
	gzipReader, err := gzip.NewReader(bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(gzipReader)
}
