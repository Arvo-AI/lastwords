package gopprof

import (
	"bytes"
	"compress/gzip"
	"runtime"
	"runtime/pprof"
	"testing"
)

func TestTopInUseRefusesGzipBomb(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zero := make([]byte, 1<<20)
	for i := 0; i < (maxProfileRaw>>20)+2; i++ {
		_, _ = zw.Write(zero)
	}
	_ = zw.Close()
	if _, _, err := TopInUse(buf.Bytes(), 10); err == nil {
		t.Fatal("a profile that inflates past the limit should be refused")
	}
}

var keep [][]byte

func TestTopInUseReadsARealHeapProfile(t *testing.T) {
	runtime.MemProfileRate = 1
	defer func() { runtime.MemProfileRate = 512 * 1024 }()
	for i := 0; i < 64; i++ {
		keep = append(keep, make([]byte, 1<<16))
	}
	runtime.GC()
	var buf bytes.Buffer
	if err := pprof.Lookup("heap").WriteTo(&buf, 0); err != nil {
		t.Fatal(err)
	}
	sites, total, err := TopInUse(buf.Bytes(), 5)
	if err != nil || len(sites) == 0 || total < 64<<16 {
		t.Fatalf("sites=%d total=%d err=%v", len(sites), total, err)
	}
}
