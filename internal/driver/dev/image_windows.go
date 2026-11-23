package dev

import (
	"debug/pe"
	"sync"
)

// imageOverhead is the size of the writable sections (.data, .bss, ...) of the executable at path. Windows
// charges them in full to the private memory of every process of that image as soon as it is loaded,
// whether they are touched or not (Linux counts only touched pages). Since Go 1.26 every Go program that
// imports crypto/rand has a 32 MiB .noptrbss buffer (crypto/internal/fips140/drbg.memory) that is meant to
// cost nothing until first use, so tsbox-init (the tsbox binary) shows about 33 MiB more private memory
// than it uses. The dev driver does not count this part against the box limit; init's heap still counts.
// 0 if the file cannot be read.
func imageOverhead(path string) int64 {
	if v, ok := overheads.Load(path); ok {
		return v.(int64)
	}
	n := readImageOverhead(path)
	overheads.Store(path, n)
	return n
}

var overheads sync.Map // path → int64

func readImageOverhead(path string) int64 {
	f, err := pe.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	const memWrite = 0x80000000 // IMAGE_SCN_MEM_WRITE
	var n int64
	for _, s := range f.Sections {
		if s.Characteristics&memWrite != 0 {
			n += (int64(s.VirtualSize) + 4095) &^ 4095
		}
	}
	return n
}
