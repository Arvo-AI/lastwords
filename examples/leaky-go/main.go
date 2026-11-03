// A Go service whose response cache grows without bound.
package main

import (
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"
	"sync"
	"time"
)

type responseCache struct {
	mu      sync.Mutex
	entries [][]byte
}

func (c *responseCache) store(b []byte) {
	c.mu.Lock()
	c.entries = append(c.entries, b)
	c.mu.Unlock()
}

var cache responseCache

func renderInvoice(n int) []byte {
	b := make([]byte, 128*1024)
	line := []byte(fmt.Sprintf("invoice %d line item; ", n))
	for i := 0; i < len(b); i += len(line) {
		copy(b[i:], line)
	}
	return b
}

func main() {
	go func() { log.Println(http.ListenAndServe(":6060", nil)) }()
	rate, _ := strconv.Atoi(os.Getenv("REQUESTS_PER_SECOND"))
	if rate == 0 {
		rate = 40
	}
	warm, _ := strconv.Atoi(os.Getenv("WARMUP_SECONDS"))
	if warm == 0 {
		warm = 20
	}
	log.Printf("billing-api started, warming up for %ds", warm)
	time.Sleep(time.Duration(warm) * time.Second)
	for n := 0; ; n++ {
		cache.store(renderInvoice(n))
		if n%500 == 0 {
			log.Printf("rendered %d invoices", n)
		}
		time.Sleep(time.Second / time.Duration(rate))
	}
}
