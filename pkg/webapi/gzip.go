package webapi

import (
	"compress/gzip"
	"io"
)

func gzipReader(r io.Reader) (*gzip.Reader, error) { return gzip.NewReader(r) }
