package core

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// AI module log. The portal's AI Agent page works out what happened on each AI call (OpenAI
// refused the token, no credit, no connection) by grepping the FreeSWITCH log for the call's
// uuid (app/library/AiVoice.php: callResults, summarize). When "ai-log-file" is set in
// <settings>, the AI streams append lines of the same shape to that file:
//
//	<uuid> 2026-11-14 03:20:48.063000 0.00% [ERR] openai_audio_streamer_glue.cpp:0 (<uuid>) processMessage - error: {...}
//
// Nothing is written when the setting is empty.

var aiLogMu sync.Mutex

func (c *Core) aiLog(level, uuid, msg string) {
	path := c.config().Settings.AILogFile
	if path == "" {
		return
	}
	msg = strings.ReplaceAll(strings.ReplaceAll(msg, "\r", " "), "\n", " ")
	line := fmt.Sprintf("%s %s 0.00%% [%s] openai_audio_streamer_glue.cpp:0 %s\n", uuid, time.Now().Format("2006-01-02 15:04:05.000000"), level, msg)
	aiLogMu.Lock()
	defer aiLogMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}
