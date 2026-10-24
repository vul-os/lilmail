package api

import (
	"log"
	"os"
	"time"
)

// timingEnabled gates the connect-vs-command latency log lines. Set
// LILMAIL_TIMING=1 to see where request time goes (IMAP connect+login or the
// command itself) without adding noise to normal logs.
var timingEnabled = os.Getenv("LILMAIL_TIMING") == "1"

// logTiming logs how long op took since start when LILMAIL_TIMING=1.
func logTiming(op string, start time.Time) {
	if timingEnabled {
		log.Printf("timing: %s took %v", op, time.Since(start).Round(time.Millisecond))
	}
}
