package store

import (
	"os"
	"time"
)

// readFileRetryAttempts and readFileRetryDelay bound how long a read waits out a
// concurrent atomic replacement of the same path.
const (
	readFileRetryAttempts = 25
	readFileRetryDelay    = 20 * time.Millisecond
)

// readFile reads a file that another process may be replacing atomically.
//
// SaveRoom and the other durable writers publish state by renaming a temporary
// file over the destination. POSIX replaces the directory entry without ever
// making the old contents unreadable, but Windows briefly denies concurrent
// opens while the replacement completes, so a reader can observe a transient
// sharing violation for a file that exists and is readable. Retry those errors
// for a bounded window and surface every other failure unchanged.
func readFile(path string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		data, err := os.ReadFile(path)
		if err == nil || attempt >= readFileRetryAttempts || !transientSharingViolation(err) {
			return data, err
		}
		time.Sleep(readFileRetryDelay)
	}
}

// replaceFile publishes a temporary file over its destination.
//
// A concurrent reader can hold the destination open on Windows just long enough
// to deny the replacement, which is the mirror image of the condition readFile
// waits out. Retry those failures for the same bounded window so a save is not
// lost to a read that is already finishing.
func replaceFile(tmpName, destination string) error {
	for attempt := 0; ; attempt++ {
		err := os.Rename(tmpName, destination)
		if err == nil || attempt >= readFileRetryAttempts || !transientSharingViolation(err) {
			return err
		}
		time.Sleep(readFileRetryDelay)
	}
}
