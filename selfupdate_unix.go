//go:build !windows

package main

import (
	"os"
	"syscall"
)

// execSelf ersetzt den laufenden Prozess durch das neue Programm (gleiche PID,
// gleiche Argumente und Umgebung). Offene Sockets werden dabei geschlossen.
func execSelf(path string) error {
	return syscall.Exec(path, os.Args, os.Environ())
}
