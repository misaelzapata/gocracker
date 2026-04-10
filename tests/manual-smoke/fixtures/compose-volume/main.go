package main

import (
	"os"
	"syscall"
	"time"
)

func main() {
	if err := os.MkdirAll("/data", 0755); err != nil {
		panic(err)
	}
	if err := os.WriteFile("/data/result.txt", []byte("compose-volume\n"), 0644); err != nil {
		panic(err)
	}
	syscall.Sync()
	time.Sleep(300 * time.Second)
}
