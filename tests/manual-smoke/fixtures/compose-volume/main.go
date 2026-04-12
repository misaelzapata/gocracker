package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if err := writeResult("/data/result.txt", "compose-volume\n"); err != nil {
		panic(err)
	}
	syscall.Sync()
	time.Sleep(300 * time.Second)
}

func writeResult(path, value string) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value), 0644)
}
