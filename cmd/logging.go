package main

import (
	"log"
	"os"
	"strings"
	"sync/atomic"
)

// logSeverity is the severity of a log line. The configured threshold is the least
// severe level that is emitted; lines with severity below the threshold are dropped.
type logSeverity int

const (
	sevDebug logSeverity = iota
	sevInfo
	sevWarn
	sevError
)

// logThreshold stores the minimum severity to print (same scale as logSeverity).
var logThreshold atomic.Uint32

func init() {
	logThreshold.Store(uint32(sevInfo))
}

func setLogThreshold(s logSeverity) {
	logThreshold.Store(uint32(s))
}

func shouldLog(msg logSeverity) bool {
	return msg >= logSeverity(logThreshold.Load())
}

// parseLogLevelString maps user-facing names to a threshold. Unknown values default to info.
func parseLogLevelString(s string) logSeverity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "d":
		return sevDebug
	case "info", "i":
		return sevInfo
	case "warn", "warning", "w":
		return sevWarn
	case "error", "e", "fatal":
		return sevError
	default:
		return sevInfo
	}
}

func isKnownLogLevelToken(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "d", "info", "i", "warn", "warning", "w", "error", "e", "fatal":
		return true
	default:
		return false
	}
}

// syncLogLevelFromData applies logLevel from ConfigMap data when set, otherwise
// CASTAI_PDB_CONTROLLER_LOG_LEVEL, otherwise "info". Call with nil when resetting CM state.
func syncLogLevelFromData(cmData map[string]string) {
	var raw string
	if cmData != nil {
		raw = strings.TrimSpace(cmData["logLevel"])
	}
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("CASTAI_PDB_CONTROLLER_LOG_LEVEL"))
	}
	var thr logSeverity
	if raw == "" {
		thr = sevInfo
	} else if !isKnownLogLevelToken(raw) {
		setLogThreshold(sevInfo)
		logWarnf("WARNING: invalid logLevel %q, using info", raw)
		return
	} else {
		thr = parseLogLevelString(raw)
	}
	setLogThreshold(thr)
}

func logDebugf(format string, args ...interface{}) {
	if !shouldLog(sevDebug) {
		return
	}
	log.Printf("DEBUG: "+format, args...)
}

func logInfof(format string, args ...interface{}) {
	if !shouldLog(sevInfo) {
		return
	}
	log.Printf(format, args...)
}

func logWarnf(format string, args ...interface{}) {
	if !shouldLog(sevWarn) {
		return
	}
	log.Printf(format, args...)
}

func logErrorf(format string, args ...interface{}) {
	if !shouldLog(sevError) {
		return
	}
	log.Printf(format, args...)
}
