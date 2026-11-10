// parity prints the Go port's results for test/parity/scenario.json (see internal/parity).
//
//	go run ./cmd/parity <work folder holding survey.csv>
package main

import (
	"fmt"
	"os"

	"signal-atlas/internal/parity"
)

func main() {
	here, _ := os.Getwd()
	text, err := parity.Run(os.Args[1], here)
	if err != nil {
		panic(err)
	}
	fmt.Print(text)
}
