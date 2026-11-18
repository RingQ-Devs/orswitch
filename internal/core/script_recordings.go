package core

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ringq-devs/orswitch/internal/sip"
)

// recordings.lua (*732): record a greeting or prompt from a phone and save it in the domain's
// recordings folder and in v_recordings, where the portal lists it.
//
// The caller enters the recording password (setting recordings/recording_password, else the
// call's pin_number) when there is one, and an id number (unless the dialplan set recording_id),
// speaks after the prompt and ends with #, hears the recording, and presses 1 to keep it or 2 to
// record it again. The file is <recording_prefix><id>.<record_ext> in <recordings dir>/<domain>
// (or the setting recordings/storage_path). Settings: recordings/storage_type (base64 also keeps
// the file in the row), recording_prefix, recording_password. Call variables: recording_id,
// recording_name, record_ext (wav), time_limit_secs (10800).
//
// Not as FreeSWITCH: the recording does not stop on silence (silence_thresh / silence_hits are
// ignored: # or the time limit ends it), and the http_cache storage type records to the local
// storage path.

func init() { RegisterScript("recordings", scriptRecordings) }

func scriptRecordings(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	if err := s.Answer(); err != nil {
		return err
	}
	if !s.Ready() {
		return nil
	}
	domain, domainUUID := s.Var("domain_name"), s.Var("domain_uuid")
	if domainUUID == "" {
		domainUUID = c.domainUUID(s.Ctx, domain)
	}
	storageType := c.scriptSetting(s.Ctx, domainUUID, "recordings", "storage_type", "text")
	storagePath := c.scriptSetting(s.Ctx, domainUUID, "recordings", "storage_path", "text")
	storagePath = strings.ReplaceAll(strings.ReplaceAll(storagePath, "${domain_name}", domain), "${domain_uuid}", domainUUID)
	dir := filepath.Join(c.config().Settings.RecordingsDir, domain)
	if storagePath != "" {
		dir = storagePath
	}
	pin := firstNonEmpty(c.scriptSetting(s.Ctx, domainUUID, "recordings", "recording_password", "numeric"), s.Var("pin_number"))
	if pin != "" {
		digits := s.PlayAndGetDigits(len(pin), len(pin)+1, 3, 3000*time.Millisecond, "#", "phrase:voicemail_enter_pass:#", "", `\d+`)
		if digits != pin {
			s.Log("info", "recordings: wrong password")
			s.Stream("phrase:voicemail_fail_auth:#")
			s.Hangup("NORMAL_CLEARING")
			return nil
		}
	}
	prefix := firstNonEmpty(c.scriptSetting(s.Ctx, domainUUID, "recordings", "recording_prefix", "text"), s.Var("recording_prefix"))
	ext := disaDefault(s.Var("record_ext"), "wav")
	limit := disaInt(s.Var("time_limit_secs"), 10800)
	for s.Ready() {
		id := s.Var("recording_id")
		var filename string
		switch {
		case id == "":
			s.Sleep(1000)
			id = s.PlayAndGetDigits(1, 20, 3, 3000*time.Millisecond, "#", "ivr/ivr-id_number.wav", "", `\d+`)
			s.SetVar("recording_id", id)
			filename = prefix + id + "." + ext
		case isAllDigits(id):
			filename = prefix + id + "." + ext
		default:
			filename = prefix + "." + ext
		}
		name := firstNonEmpty(s.Var("recording_name"), filename)
		s.Stream("ivr/ivr-recording_started.wav")
		s.SetVar("playback_terminators", "#")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.Log("err", "recordings folder", "dir", dir, "err", err)
			return err
		}
		path := filepath.Join(dir, filename)
		if err := s.Execute("record", path+" "+strconv.Itoa(limit)); err != nil && s.Ready() {
			s.Log("warn", "recording failed", "err", err)
		}
		recBase64 := ""
		if storageType == "base64" {
			if data, err := os.ReadFile(path); err == nil {
				recBase64 = base64.StdEncoding.EncodeToString(data)
			}
		}
		if err := saveRecordingRow(s, domainUUID, filename, name, recBase64); err != nil {
			s.Log("warn", "v_recordings not written", "err", err)
		}
		if !s.Ready() {
			return nil
		}
		s.Stream(path)
		digits := ""
		for _, st := range []struct {
			prompt  string
			min     int
			timeout time.Duration
		}{{"voicemail/vm-save_recording.wav", 0, 100 * time.Millisecond}, {"voicemail/vm-press.wav", 0, 100 * time.Millisecond},
			{"digits/1.wav", 0, 100 * time.Millisecond}, {"voicemail/vm-rerecord.wav", 0, 100 * time.Millisecond},
			{"voicemail/vm-press.wav", 0, 100 * time.Millisecond}, {"digits/2.wav", 1, 5 * time.Second}} {
			if digits != "" || !s.Ready() {
				break
			}
			digits = s.PlayAndGetDigits(st.min, 1, 1, st.timeout, "#", st.prompt, "", `\d+`)
		}
		if digits == "2" {
			_ = os.Remove(path)
			continue // record again
		}
		s.Stream("voicemail/vm-saved.wav")
		break
	}
	s.Hangup("")
	return nil
}

func isAllDigits(v string) bool {
	_, err := strconv.ParseFloat(v, 64)
	return err == nil
}

// saveRecordingRow replaces the recording's v_recordings row, keeping its description and name.
func saveRecordingRow(s *Script, domainUUID, filename, name, b64 string) error {
	c := s.Core
	desc := ""
	if rows, err := c.sqlRows(s.Ctx, `SELECT coalesce(recording_description, '') AS d, coalesce(recording_name, '') AS n FROM v_recordings
		WHERE domain_uuid = $1::uuid AND recording_filename = $2 LIMIT 1`, domainUUID, filename); err == nil && len(rows) > 0 {
		desc, name = rows[0]["d"], firstNonEmpty(rows[0]["n"], name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // the call may be gone
	defer cancel()
	if _, err := c.sqlExec(ctx, `DELETE FROM v_recordings WHERE domain_uuid = $1::uuid AND recording_filename = $2`, domainUUID, filename); err != nil {
		return err
	}
	_, err := c.sqlExec(ctx, `INSERT INTO v_recordings (recording_uuid, domain_uuid, recording_filename, recording_description, recording_base64, recording_name)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)`, sip.NewUUID(), domainUUID, filename, desc, b64, name)
	return err
}
