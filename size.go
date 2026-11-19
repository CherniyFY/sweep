package main

import (
	"io/fs"
	"os"
)

// pathSize reports the apparent size of a file or directory tree. Apparent size
// is what a user sees in their file manager; it ignores filesystem block
// overhead, so it under-reports tiny-file-heavy caches slightly.
func pathSize(root *os.Root, name string) (int64, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}

	var total int64
	err = fs.WalkDir(root.FS(), name, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		info, err := root.Lstat(path)
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// formatBytes renders a byte count compactly.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return itoa(n) + " B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	units := "KMGTPE"
	return trimFloat(n, div) + " " + string(units[exp]) + "iB"
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func trimFloat(n, div int64) string {
	whole := n / div
	frac := (n % div) * 10 / div
	if frac == 0 {
		return itoa(whole)
	}
	return itoa(whole) + "." + itoa(frac)
}
