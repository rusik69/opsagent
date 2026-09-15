package store

import "testing"

func TestFTsErr(t *testing.T) {
	s := openStore(t)
	t.Logf("ftsMemories=%v", s.ftsMemories)
}
