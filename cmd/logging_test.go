package main

import (
	"testing"
)

func TestParseLogLevelString(t *testing.T) {
	tests := []struct {
		in   string
		want logSeverity
	}{
		{"debug", sevDebug},
		{"DEBUG", sevDebug},
		{"d", sevDebug},
		{"info", sevInfo},
		{"warn", sevWarn},
		{"warning", sevWarn},
		{"error", sevError},
		{"fatal", sevError},
		{"", sevInfo},
		{"unknown-level", sevInfo},
	}
	for _, tt := range tests {
		if got := parseLogLevelString(tt.in); got != tt.want {
			t.Errorf("parseLogLevelString(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIsKnownLogLevelToken(t *testing.T) {
	if !isKnownLogLevelToken("INFO") {
		t.Error("INFO should be known")
	}
	if isKnownLogLevelToken("trace") {
		t.Error("trace should not be known")
	}
}

func TestShouldLogRespectsThreshold(t *testing.T) {
	old := logThreshold.Load()
	defer logThreshold.Store(old)

	logThreshold.Store(uint32(sevInfo))
	if shouldLog(sevDebug) {
		t.Error("debug should not log at info threshold")
	}
	if !shouldLog(sevInfo) {
		t.Error("info should log at info threshold")
	}
	if !shouldLog(sevError) {
		t.Error("error should log at info threshold")
	}

	logThreshold.Store(uint32(sevError))
	if shouldLog(sevDebug) || shouldLog(sevInfo) || shouldLog(sevWarn) {
		t.Error("only error should log at error threshold")
	}
	if !shouldLog(sevError) {
		t.Error("error should log at error threshold")
	}
}
