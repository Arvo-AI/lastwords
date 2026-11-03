// A queue consumer that panics on a message it does not expect.
package main

import (
	"log"
	"time"
)

type order struct {
	ID       string
	Discount map[string]float64
}

func applyDiscount(o *order, code string) float64 {
	return o.Discount[code] * 100 // fine for reads...
}

func registerDiscount(o *order, code string, v float64) {
	o.Discount[code] = v // ...but panics when Discount is nil
}

func main() {
	log.Println("order-consumer started")
	orders := []*order{
		{ID: "A-1001", Discount: map[string]float64{"WELCOME": 0.1}},
		{ID: "A-1002", Discount: map[string]float64{}},
		{ID: "A-1003"}, // legacy message without discounts
	}
	for _, o := range orders {
		time.Sleep(5 * time.Second)
		log.Printf("processing order %s (discount %.0f%%)", o.ID, applyDiscount(o, "WELCOME"))
		registerDiscount(o, "LOYALTY", 0.05)
	}
	select {}
}
