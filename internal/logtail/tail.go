// Package logtail reads the end of plugin log files.
package logtail

import (
	"errors"
	"io"
)

// MaxBytes bounds the raw tail and the encoded HTTP response. The reverse
// scan can read one additional byte to check for the delimiter before the tail.
const MaxBytes = 2 << 20

// ErrTooLarge means the requested tail exceeds the byte budget.
var ErrTooLarge = errors.New("requested log tail exceeds the 2 MiB limit")

// Start finds the start of the last lines in a file of the given size.
// A zero line count selects the entire file.
func Start(file io.ReaderAt, size int64, lines int) (int64, error) {
	return findStart(file, size, lines, 0)
}

// BoundedStart finds the start of the requested tail within MaxBytes.
func BoundedStart(file io.ReaderAt, size int64, lines int) (int64, error) {
	if lines == 0 {
		if size > MaxBytes {
			return 0, ErrTooLarge
		}
		return 0, nil
	}
	start, err := findStart(file, size, lines, max(0, size-MaxBytes-1))
	if err == nil && size-start > MaxBytes {
		return 0, ErrTooLarge
	}
	return start, err
}

func findStart(file io.ReaderAt, size int64, lines int, floor int64) (int64, error) {
	if lines == 0 {
		return 0, nil
	}
	const blockSize = 32 * 1024
	var block [blockSize]byte
	for end := size; end > floor; {
		start := max(floor, end-blockSize)
		part := block[:end-start]
		if _, err := file.ReadAt(part, start); err != nil {
			return 0, err
		}
		for i := len(part) - 1; i >= 0; i-- {
			position := start + int64(i)
			if part[i] == '\n' && position != size-1 {
				lines--
				if lines == 0 {
					return position + 1, nil
				}
			}
		}
		end = start
	}
	if floor > 0 {
		return 0, ErrTooLarge
	}
	return 0, nil
}
