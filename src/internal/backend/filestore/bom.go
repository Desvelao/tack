package filestore

import "bytes"

// BOM is the UTF-8 byte order mark some editors put at the start of a file.
const BOM = "\xef\xbb\xbf"

// StripBOM returns data without a leading BOM and whether it had one.
func StripBOM(data []byte) ([]byte, bool) {
	if bytes.HasPrefix(data, []byte(BOM)) {
		return data[len(BOM):], true
	}
	return data, false
}

// WithBOM returns data prefixed with a BOM when bom is true.
func WithBOM(data []byte, bom bool) []byte {
	if !bom {
		return data
	}
	return append([]byte(BOM), data...)
}
