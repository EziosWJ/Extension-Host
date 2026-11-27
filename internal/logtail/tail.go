// Package logtail reads the end of plugin log files.
package logtail

import "io"

// Start finds the start of the last lines in a file of the given size.
// A zero line count selects the entire file.
func Start(file io.ReaderAt, size int64, lines int) (int64, error) {
	if lines == 0 || size == 0 {
		return 0, nil
	}
	const blockSize = 32 * 1024
	var block [blockSize]byte
	for end := size; end > 0; {
		start := max(0, end-blockSize)
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
	return 0, nil
}
