package core

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ringq-devs/orswitch/internal/sip"
)

// More FusionPBX app scripts: conferences/transfer.lua, conference_center (conference.lua,
// mute, unmute, raise_hand, start_recording), fax hangup_rx / hangup_tx, provision and
// servers/clear_cache.

func init() {
	RegisterScript("transfer", scriptConferenceTransfer) // app/conferences/resources/transfer.lua
	RegisterAppScript("conference_center", scriptConferenceCenter)
	RegisterScript("conference", scriptConferenceCenter) // app/conference_center/conference.lua
	RegisterScript("mute", scriptConfMute)
	RegisterScript("unmute", scriptConfMute)
	RegisterScript("raise_hand", scriptRaiseHand)
	RegisterScript("start_recording", scriptConfStartRecording)
	RegisterScript("hangup_rx", scriptFaxHangup)
	RegisterScript("hangup_tx", scriptFaxHangup)
	RegisterAppScript("provision", appScriptProvision)
	RegisterScript("clear_cache", scriptClearCache)
}

// ---------------------------------------------------------------------------
// conferences/transfer.lua <extension>

// scriptConferenceTransfer moves the active call of <extension> into the conference_extension and
// follows it there (after the PIN, when pin_number is set).
func scriptConferenceTransfer(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	if err := s.Answer(); err != nil {
		return err
	}
	ext := s.Arg(1)
	domain := firstNonEmpty(s.Var("domain_name"), s.Var("sip_auth_realm"))
	confExt := s.Var("conference_extension")
	if pin := s.Var("pin_number"); pin != "" {
		d := s.PlayAndGetDigits(len(pin), len(pin)+1, 3, 5000*time.Millisecond, "#", "ivr/ivr-please_enter_pin_followed_by_pound.wav", "", `\d+`)
		if d != pin {
			s.Stream("voicemail/vm-fail_auth.wav")
			s.Hangup("NORMAL_CLEARING")
			return nil
		}
	}
	if !s.Ready() || ext == "" || confExt == "" {
		return nil
	}
	var target *Channel
	for _, ch := range c.Channels() {
		if ch != s.Ch && strings.EqualFold(ch.Var("presence_id"), ext+"@"+domain) && ch.Snapshot().CallState == "ACTIVE" {
			target = ch
			break
		}
	}
	if target == nil {
		return nil
	}
	if p := target.Partner(); p != nil {
		c.transfer(p, confExt, domain)
	}
	c.transfer(target, confExt, domain)
	return s.Transfer(confExt, domain)
}

// ---------------------------------------------------------------------------
// conference_center

// scriptConfMute (mute.lua / unmute.lua <member>): mutes or unmutes a member of the caller's conference.
func scriptConfMute(s *Script) error {
	if s.Ch == nil {
		return nil
	}
	cmd := "mute"
	if s.Name == "unmute" {
		cmd = "unmute"
	}
	s.Core.API("conference " + s.Var("conference_name") + " " + cmd + " " + s.Arg(1))
	return nil
}

// scriptRaiseHand (raise_hand.lua true|false|toggle): the call's hand_raised variable.
func scriptRaiseHand(s *Script) error {
	if s.Ch == nil {
		return nil
	}
	switch v := s.Arg(1); v {
	case "toggle":
		if s.Var("hand_raised") == "true" {
			s.SetVar("hand_raised", "false")
		} else {
			s.SetVar("hand_raised", "true")
		}
	case "true", "false":
		s.SetVar("hand_raised", v)
	}
	return nil
}

// scriptConfStartRecording (start_recording.lua <meeting uuid> <domain> <ext>): starts recording a
// running meeting into <recordings>/<domain>/archive/<yyyy>/<Mon>/<dd>/<session uuid>.<ext>.
func scriptConfStartRecording(s *Script) error {
	meeting, domain, ext := s.Arg(1), s.Arg(2), disaDefault(s.Arg(3), "wav")
	room := s.Core.confRoomFor(domain, meeting)
	if room == nil {
		return nil
	}
	now := time.Now()
	dir := filepath.Join(s.Core.config().Settings.RecordingsDir, domain, "archive", now.Format("2006"), now.Format("Jan"), now.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file := filepath.Join(dir, room.key+"."+ext)
	if _, err := os.Stat(file); err == nil {
		return nil
	}
	s.Core.API("conference " + meeting + "@" + domain + " record " + file)
	return nil
}

func (c *Core) confRoomFor(domain, name string) *confRoom {
	c.conf.mu.Lock()
	defer c.conf.mu.Unlock()
	for _, r := range c.conf.rooms {
		if strings.EqualFold(r.name, name) && (domain == "" || strings.EqualFold(r.domain, domain)) {
			return r
		}
	}
	return nil
}

// scriptConferenceCenter (app/conference_center/conference.lua): a caller dials the conference
// center's number, enters a moderator or participant PIN (three tries) and joins the matching
// room (v_conference_rooms): flags (mute for participants, moderator, endconf), announcements,
// recording, and the session / member rows (v_conference_sessions, v_conference_session_details)
// when the conference_center/session_enabled setting is on.
//
// Not as the Lua: the copy on the reference server has its PIN lookup commented out (nobody can
// join); this one looks the PIN up as FusionPBX does. wait-mod, endconf and moderator flags are
// passed on but the built-in mixer ignores all but mute and record.
func scriptConferenceCenter(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	if err := s.Answer(); err != nil {
		return err
	}
	s.Sleep(1000)
	domain := s.Var("domain_name")
	domainUUID := s.DomainUUID("")
	dest, cidNum := s.Var("destination_number"), s.Var("caller_id_number")
	sessionEnabled := c.scriptSetting(s.Ctx, domainUUID, "conference_center", "session_enabled", "boolean") == "true"
	accountCode := c.scriptSetting(s.Ctx, domainUUID, "conference_center", "account_code_enabled", "boolean") == "true"
	centerUUID, greeting := "", ""
	if rows, err := c.sqlRows(s.Ctx, `SELECT conference_center_uuid::text AS u, coalesce(conference_center_greeting, '') AS g FROM v_conference_centers
		WHERE domain_uuid = $1::uuid AND conference_center_extension = $2`, domainUUID, dest); err == nil && len(rows) > 0 {
		centerUUID, greeting = rows[0]["u"], rows[0]["g"]
	}
	greeting = disaDefault(greeting, "conference/conf-pin.wav")
	var room map[string]string
	pin := s.Var("pin_number")
	for try := 0; try < 3 && room == nil && s.Ready(); try++ {
		p := pin
		if p == "" {
			p = s.PlayAndGetDigits(2, 20, 1, 5*time.Second, "#", greeting, "", `\d+`)
			p = digitsOnly(p)
		}
		if p != "" {
			if rows, err := c.sqlRows(s.Ctx, `SELECT conference_room_uuid::text AS u, lower(conference_room_name) AS name, lower(coalesce(record, '')) AS record,
				lower(coalesce(profile, '')) AS profile, coalesce(max_members::text, '') AS max_members, coalesce(wait_mod, '') AS wait_mod,
				coalesce(moderator_endconf, '') AS endconf, coalesce(moderator_pin, '') AS mpin, coalesce(participant_pin, '') AS ppin,
				coalesce(announce_name, '') AS announce_name, coalesce(announce_count, '') AS announce_count,
				coalesce(announce_recording, '') AS announce_recording, coalesce(mute, '') AS mute, coalesce(sounds, '') AS sounds
				FROM v_conference_rooms WHERE domain_uuid = $1::uuid AND ($3 = '' OR conference_center_uuid = $3::uuid)
				AND (moderator_pin = $2 OR participant_pin = $2) AND enabled = 'true'
				AND (start_datetime IS NULL OR start_datetime = '' OR start_datetime <= $4)
				AND (stop_datetime IS NULL OR stop_datetime = '' OR stop_datetime > $4)`,
				domainUUID, p, centerUUID, time.Now().Format("2006-01-02 15:04:05")); err == nil && len(rows) > 0 {
				room = rows[0]
				pin = p
				break
			}
		}
		pin = ""
		if try < 2 {
			s.Stream("conference/conf-bad-pin.wav")
		}
	}
	if room == nil {
		s.Stream("conference/conf-bad-pin.wav")
		s.Hangup("NORMAL_CLEARING")
		return nil
	}
	moderator := pin == room["mpin"]
	if accountCode && moderator {
		code := s.PlayAndGetDigits(2, 20, 3, 5*time.Second, "#", "phrase:voicemail_enter_id:#", "", `\d+`)
		if _, err := c.sqlExec(s.Ctx, `UPDATE v_conference_rooms SET account_code = $1 WHERE conference_center_uuid = $2::uuid`, code, centerUUID); err != nil {
			s.Log("warn", "account code not saved", "err", err)
		}
	}
	roomUUID := room["u"]
	s.SetVar("conference_room_uuid", roomUUID)
	s.SetVar("conference_room_name", room["name"])
	if n, _ := strconv.Atoi(room["max_members"]); n > 0 {
		s.SetVar("conference_max_members", room["max_members"])
		if r := c.confRoomFor(domain, roomUUID); r != nil {
			r.mu.Lock()
			count := len(r.members)
			r.mu.Unlock()
			if count >= n {
				s.Stream("conference/conf-locked.wav")
				s.Hangup("CALL_REJECTED")
				return nil
			}
		}
	}
	if room["sounds"] != "true" {
		s.SetVar("conference_enter_sound", "none")
		s.SetVar("conference_exit_sound", "none")
	}
	flags := ""
	if moderator {
		flags = "|moderator"
		if room["endconf"] == "true" {
			flags += "|endconf"
		}
	} else {
		if room["wait_mod"] == "true" {
			flags += "wait-mod"
		}
		if room["mute"] == "true" {
			flags += "|mute"
		}
	}
	record := room["record"] == "true"
	if record {
		flags += "|record"
		if room["announce_recording"] == "true" {
			s.Stream("ivr/ivr-recording_started.wav")
		}
	}
	start := time.Now()
	sessionUUID := s.UUID() // the Lua's conference_uuid: the first member's call
	if sessionEnabled {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			end := time.Now()
			if rows, err := c.sqlRows(ctx, `SELECT count(*)::text AS n FROM v_conference_sessions WHERE conference_session_uuid = $1::uuid`, sessionUUID); err == nil && len(rows) > 0 && rows[0]["n"] == "0" {
				_, _ = c.sqlExec(ctx, `INSERT INTO v_conference_sessions (conference_session_uuid, domain_uuid, meeting_uuid, profile) VALUES ($1::uuid, $2::uuid, $3::uuid, $4)`,
					sessionUUID, domainUUID, roomUUID, room["profile"])
			}
			var mod any
			if moderator {
				mod = "true"
			}
			if _, err := c.sqlExec(ctx, `INSERT INTO v_conference_session_details (conference_session_detail_uuid, domain_uuid, conference_session_uuid, meeting_uuid,
				username, caller_id_name, caller_id_number, network_addr, uuid, moderator, start_epoch, end_epoch)
				VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9::uuid, $10, $11, $12)`,
				sip.NewUUID(), domainUUID, sessionUUID, roomUUID, s.Var("username"), s.Var("caller_id_name"), cidNum, s.Var("network_addr"),
				s.UUID(), mod, start.Unix(), end.Unix()); err != nil {
				s.Log("warn", "conference member not logged", "err", err)
			}
		}()
	}
	profile := disaDefault(room["profile"], "default")
	return s.Execute("conference", roomUUID+"@"+domain+"@"+profile+"+flags{"+flags+"}")
}

// ---------------------------------------------------------------------------
// fax hangup_rx / hangup_tx (api_hangup_hook of the FusionPBX fax dialplans)

// scriptFaxHangup logs a finished fax like hangup_rx.lua / hangup_tx.lua: a v_fax_logs row, a
// v_fax_files row for a received (or successfully sent) fax, the v_fax_queue status of a sent
// one, and the mail to the fax extension's address (queued; fax_to_email.php is not run, the fax
// file is attached). Run it with the call's variables: api_hangup_hook=lua hangup_rx.lua.
func scriptFaxHangup(s *Script) error {
	c := s.Core
	tx := s.Name == "hangup_tx"
	h := s.Header
	domainUUID := h("domain_uuid")
	if d := h("domain_name"); d != "" {
		if u := c.domainUUID(s.Ctx, d); u != "" {
			domainUUID = u
		}
	}
	faxUUID, queueUUID, callUUID := h("fax_uuid"), h("fax_queue_uuid"), h("uuid")
	success := disaDefault(h("fax_success"), "0")
	text := disaDefault(h("fax_result_text"), "FS_NOT_SET")
	if h("hangup_cause_q850") == "17" {
		success, text = "0", "USER_BUSY"
	}
	file := firstNonEmpty(h("fax_file"), h("fax_filename"))
	cidName := firstNonEmpty(h("caller_id_name"), h("Caller-Caller-ID-Name"))
	cidNum := firstNonEmpty(h("caller_id_number"), h("Caller-Caller-ID-Number"))
	var email, extension, forward, prefix, fcidName, fcidNum string
	if rows, err := c.sqlRows(s.Ctx, `SELECT coalesce(fax_email, '') AS e, coalesce(fax_extension, '') AS x, coalesce(fax_forward_number, '') AS f,
		coalesce(fax_prefix::text, '') AS p, coalesce(fax_caller_id_name, '') AS n, coalesce(fax_caller_id_number, '') AS num
		FROM v_fax WHERE fax_uuid = $1::uuid AND domain_uuid = $2::uuid`, faxUUID, domainUUID); err == nil && len(rows) > 0 {
		r := rows[0]
		email, extension, forward, prefix, fcidName, fcidNum = r["e"], r["x"], r["f"], r["p"], r["n"], r["num"]
	}
	_, _ = forward, prefix
	if !tx && email != "" {
		subject := "Fax received: " + file[strings.LastIndex(file, "/")+1:]
		body := "result:" + text + " sender:" + h("fax_remote_station_id") + " pages:" + h("fax_document_total_pages")
		if err := c.QueueEmail(s.Ctx, EmailMessage{DomainUUID: domainUUID, Type: "fax", CallUUID: callUUID, To: email, Subject: subject, Body: body, Attachment: file}); err != nil {
			s.Log("warn", "fax mail not queued", "err", err)
		}
	}
	num := func(v string) any {
		if v == "" {
			return "0"
		}
		return v
	}
	cols := `fax_log_uuid, domain_uuid, fax_uuid, fax_success, fax_result_code, fax_result_text, fax_file, fax_ecm_used, fax_local_station_id,
		fax_document_transferred_pages, fax_document_total_pages, fax_image_resolution, fax_image_size, fax_bad_rows, fax_transfer_rate, fax_uri, fax_date, fax_epoch`
	vals := `$1::uuid, $2::uuid, NULLIF($3, '')::uuid, $4, $5, $6, $7, $8, $9, $10::numeric, $11::numeric, $12, $13, $14, $15, $16, now(), $17::numeric`
	args := []any{callUUID, domainUUID, faxUUID, success, h("fax_result_code"), text, file, h("fax_ecm_used"), h("fax_local_station_id"),
		num(h("fax_document_transferred_pages")), num(h("fax_document_total_pages")), h("fax_image_resolution"), h("fax_image_size"),
		h("fax_bad_rows"), h("fax_transfer_rate"), h("fax_uri"), strconv.FormatInt(time.Now().Unix(), 10)}
	if tx {
		cols += ", fax_duration"
		vals += ", $18::numeric"
		args = append(args, num(h("billsec")))
	}
	if _, err := c.sqlExec(s.Ctx, `INSERT INTO v_fax_logs (`+cols+`) VALUES (`+vals+`)`, args...); err != nil {
		s.Log("warn", "fax log not written", "err", err)
	}
	if tx && queueUUID != "" {
		status := "trying"
		if success == "1" {
			status = "sent"
		}
		if rc := h("fax_result_code"); rc == "2" || rc == "3" || h("hangup_cause_q850") == "17" {
			status = "busy"
		}
		if _, err := c.sqlExec(s.Ctx, `UPDATE v_fax_queue SET fax_status = $1, fax_log_uuid = $2::uuid WHERE fax_queue_uuid = $3::uuid`, status, callUUID, queueUUID); err != nil {
			s.Log("warn", "fax queue not updated", "err", err)
		}
	}
	if success == "1" && faxUUID != "" {
		mode, name, number, dest := "rx", cidName, cidNum, ""
		if tx {
			mode, name, number, dest = "tx", fcidName, fcidNum, h("sip_to_user")
		}
		q := `INSERT INTO v_fax_files (fax_file_uuid, fax_uuid, fax_mode, fax_file_type, fax_file_path, fax_caller_id_name, fax_caller_id_number, fax_destination, fax_date, fax_epoch, domain_uuid)
			VALUES ($1::uuid, $2::uuid, $3, 'tif', $4, $5, $6, $7, now(), $8::numeric, $9::uuid)`
		if _, err := c.sqlExec(s.Ctx, q, callUUID, faxUUID, mode, file, name, number, dest, strconv.FormatInt(time.Now().Unix(), 10), domainUUID); err != nil {
			s.Log("warn", "fax file not written", "err", err)
		}
	}
	_ = extension
	return nil
}

// ---------------------------------------------------------------------------
// provision: hot-desking login from a phone (app/provision)

// appScriptProvision (app.lua provision, variable action=login|logout): the caller enters a device
// id and password; with login the phone they dialled from is set to take over that device's
// lines (v_devices.device_uuid_alternate) and is told to re-provision. Needs the phone's
// registration (sip_from_uri) and the call's domain_uuid.
func appScriptProvision(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	if err := s.Answer(); err != nil {
		return err
	}
	s.Sleep(500)
	domainUUID, action := s.Var("domain_uuid"), s.Var("action")
	reboot := disaDefault(s.Var("reboot"), "true")
	userID := s.PlayAndGetDigits(2, 20, 3, 5*time.Second, "#", "phrase:voicemail_enter_id:#", "", `\d+`)
	password := s.PlayAndGetDigits(2, 20, 3, 5*time.Second, "#", "phrase:voicemail_enter_pass:#", "", `\d+`)
	uri := strings.TrimPrefix(strings.TrimPrefix(s.Var("sip_from_uri"), "sip:"), "sips:")
	user, rest, _ := strings.Cut(uri, "@")
	domain, _, _ := strings.Cut(rest, ":")
	var deviceUUID, alternate string
	if user != "" && domain != "" && domainUUID != "" {
		if rows, err := c.sqlRows(s.Ctx, `SELECT device_uuid::text AS d FROM v_device_lines WHERE user_id = $1 AND server_address = $2 AND domain_uuid = $3::uuid LIMIT 1`, user, domain, domainUUID); err == nil && len(rows) > 0 {
			deviceUUID = rows[0]["d"]
		}
	}
	if userID != "" && password != "" && domainUUID != "" {
		if rows, err := c.sqlRows(s.Ctx, `SELECT device_uuid::text AS d FROM v_devices WHERE device_username = $1 AND device_password = $2 AND domain_uuid = $3::uuid LIMIT 1`, userID, password, domainUUID); err == nil && len(rows) > 0 {
			alternate = rows[0]["d"]
		}
	}
	if alternate == "" {
		s.Stream("voicemail/vm-fail_auth.wav")
		return nil
	}
	notify := func(u, d string) {
		event := "check-sync;reboot=" + reboot
		for _, b := range c.reg.Lookup(u, d) {
			go c.notify(b, event, "", nil)
		}
	}
	switch action {
	case "login":
		if deviceUUID != "" {
			// the phone that was logged in on this device goes back to its own lines
			if rows, err := c.sqlRows(s.Ctx, `SELECT user_id, server_address FROM v_device_lines WHERE device_uuid = $1::uuid AND domain_uuid = $2::uuid`, alternate, domainUUID); err == nil {
				for _, r := range rows {
					if _, err := c.sqlExec(s.Ctx, `UPDATE v_devices SET device_uuid_alternate = NULL WHERE device_uuid_alternate = $1::uuid AND domain_uuid = $2::uuid`, alternate, domainUUID); err != nil {
						s.Log("warn", "device not released", "err", err)
					}
					notify(r["user_id"], r["server_address"])
				}
			}
			s.Hangup("")
			if _, err := c.sqlExec(s.Ctx, `UPDATE v_devices SET device_uuid_alternate = $1::uuid WHERE device_uuid = $2::uuid AND domain_uuid = $3::uuid`, alternate, deviceUUID, domainUUID); err != nil {
				s.Log("warn", "device not assigned", "err", err)
			}
		}
	case "logout":
		if deviceUUID != "" {
			if _, err := c.sqlExec(s.Ctx, `UPDATE v_devices SET device_uuid_alternate = NULL WHERE device_uuid_alternate = $1::uuid AND domain_uuid = $2::uuid`, deviceUUID, domainUUID); err != nil {
				s.Log("warn", "device not released", "err", err)
			}
		}
	}
	s.Hangup("")
	notify(user, domain)
	return nil
}

// ---------------------------------------------------------------------------
// servers/clear_cache

// scriptClearCache (app/servers/resources/clear_cache.lua flush | delete <file>): empties the
// FusionPBX cache folder (setting cache/location), if there is one. RingQswitch has no file
// cache of its own: a flush also makes it read its configuration again.
func scriptClearCache(s *Script) error {
	c := s.Core
	dir := c.scriptSetting(s.Ctx, "", "cache", "location", "text")
	cmd, file := strings.TrimSpace(s.Arg(1)), strings.TrimSpace(s.Arg(2))
	switch cmd {
	case "flush":
		if dir != "" {
			if files, err := filepath.Glob(filepath.Join(dir, "*")); err == nil {
				for _, f := range files {
					if st, err := os.Stat(f); err == nil && !st.IsDir() {
						_ = os.Remove(f)
					}
				}
			}
		}
		c.requestReload()
	case "delete":
		if dir != "" && file != "" && filepath.Base(file) == file {
			_ = os.Remove(filepath.Join(dir, file))
		}
		c.requestReload()
	}
	return nil
}
