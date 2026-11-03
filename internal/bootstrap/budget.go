package bootstrap

// Aggregate limits include rejected candidates and repeated aliases, so a home
// with many individually bounded entries cannot make discovery read indefinitely.
type scanBudget struct {
	servers int
	skills  int
	entries int
	bytes   int64
}

func newScanBudget() *scanBudget {
	return &scanBudget{servers: 1024, skills: 1024, entries: 100000, bytes: 128 << 20}
}

func (b *scanBudget) limit(perFile int64) int64 {
	if b != nil && b.bytes < perFile {
		return b.bytes
	}
	return perFile
}

func (b *scanBudget) charge(size int) {
	if b != nil {
		b.bytes -= int64(size)
	}
}
