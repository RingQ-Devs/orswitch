package core

// SetLiveTranscribeBridge points live_transcribe.lua at another websocket (tests).
func SetLiveTranscribeBridge(u string) (restore func()) {
	old := liveTranscribeBridge
	liveTranscribeBridge = u
	return func() { liveTranscribeBridge = old }
}

// ValidStreamURI exposes the module's websocket uri check (tests).
func ValidStreamURI(u string) bool { return validStreamURI(u) }
