// Package xlog is a tiny leveled logger used internally by the SDK.
//
// It writes to the standard log package by default. Embedding applications
// that need a different format or destination can configure the standard
// logger or replace the output entirely.
package xlog

import (
	"log"
	"strings"
	"sync/atomic"
)

// Level is the minimum severity that is written to the log.
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelOff
)

// level defaults to LevelInfo; lower levels are dropped unless the embedding
// application opts in with SetLevel (for example via ParseLevel(os.Getenv(...))).
var level atomic.Int32

func init() {
	level.Store(int32(LevelInfo))
}

// SetLevel sets the minimum severity that is logged.
func SetLevel(l Level) {
	level.Store(int32(l))
}

// CurrentLevel returns the configured minimum severity.
func CurrentLevel() Level {
	return Level(level.Load())
}

// ParseLevel maps a level name to a Level. Unknown names fall back to
// LevelInfo, so a typo never silences the logs.
func ParseLevel(name string) Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	case "off", "none", "silent":
		return LevelOff
	default:
		return LevelInfo
	}
}

// Debug logs a debug message, for example per-request context size breakdowns.
func Debug(format string, v ...any) {
	logAt(LevelDebug, "[DEBUG] ", format, v...)
}

// Info logs an informational message.
func Info(format string, v ...any) {
	logAt(LevelInfo, "[INFO] ", format, v...)
}

// Warn logs a warning message.
func Warn(format string, v ...any) {
	logAt(LevelWarn, "[WARN] ", format, v...)
}

// Error logs an error message.
func Error(format string, v ...any) {
	logAt(LevelError, "[ERROR] ", format, v...)
}

func logAt(messageLevel Level, prefix, format string, v ...any) {
	if Level(level.Load()) > messageLevel {
		return
	}
	log.Printf(prefix+format, v...)
}
