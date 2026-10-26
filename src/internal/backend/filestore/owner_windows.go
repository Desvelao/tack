//go:build windows

package filestore

import "os"

// copyOwner: files have no Unix owner and group here.
func copyOwner(string, os.FileInfo) {}
