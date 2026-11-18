package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ringq-devs/orswitch/internal/config"
	"github.com/ringq-devs/orswitch/internal/sip"
)

// What the FusionPBX voicemail app (app/voicemail, 34 Lua files) does that the built-in voicemail
// (pbx.go, phonekeys.go) did not, and is added here:
//
//   - mailbox authentication when the mailbox is not the caller's own (check_password.lua,
//     get_voicemail_id.lua): the mailbox id and password are asked, three tries
//   - voicemail to email (send_email.lua): the message is queued as a mail to voicemail_mail_to
//     from the "voicemail" email template, with the recording attached for voicemail_file=attach,
//     and deleted again for voicemail_local_after_email=false
//   - voicemail destinations (v_voicemail_destinations): copies of each message in the other mailboxes
//   - the mailbox-full check of the vm_disk_quota variable
//
// Still missing (REPORT.md): the main menu with the advanced menu (greeting recording and choice,
// name, password change), forwarding a message to an extension, replaying the caller id and time,
// the recording menu after leaving a message, voicemail to SMS, the tutorial, message_order and
// the say-caller-id / say-date-time settings, transcription in the mail.

// vmAuthenticate decides whether the caller may listen to the mailbox id. A user who is signed in
// as the mailbox's own extension just listens (FusionPBX: voicemail_authorized); everyone else
// (a phone dialling 7878 or *98, an extension asking for another box) enters the mailbox id when
// none is given and then its password. The global variable ringq_vm_always_password=true asks the
// owner too, as the reference server's dialplans do. It returns nil when the call is to end.
func (c *Core) vmAuthenticate(ctx context.Context, ch *Channel, domain, id string) *config.Voicemail {
	own := id != "" && (id == ch.Var("user_name") || id == ch.Var("sip_number_alias"))
	authorized := truthy(ch.Var("voicemail_authorized")) || ch.Var("sip_authorized") == "true"
	if own && authorized && !truthy(c.globalVar("ringq_vm_always_password")) {
		return c.config().Features.Voicemail(id, domain)
	}
	s := &Script{Core: c, Ch: ch, Ctx: ctx, Name: "voicemail"}
	timeouts := 0
	for id == "" {
		ch.FlushDTMF()
		id = s.PlayAndGetDigits(1, 20, 3, 5*time.Second, "#", "phrase:voicemail_enter_id:#", "", `\d+`)
		if id == "" {
			if timeouts++; timeouts >= 3 || !s.Ready() {
				s.Stream("phrase:voicemail_goodbye")
				ch.Hangup(CauseNormalClearing)
				return nil
			}
		}
	}
	vm := c.config().Features.Voicemail(id, domain)
	if vm == nil || !vm.Enabled {
		// the digits a caller typed are not logged: at a mailbox prompt people sometimes key in their PIN
		ch.log.Info("voicemail: no such enabled mailbox", "digits", len(id), "domain", domain)
		ch.Hangup(CauseNormalClearing)
		return nil
	}
	for tries := 0; tries < 3 && s.Ready(); tries++ {
		pw := s.PlayAndGetDigits(2, 20, 3, 5*time.Second, "#", "phrase:voicemail_enter_pass:#", "", `\d+`)
		if vm.Password != "" && pw == vm.Password {
			return vm
		}
		s.Stream("phrase:voicemail_fail_auth")
	}
	if s.Ready() {
		s.Stream("phrase:voicemail_goodbye")
		ch.Hangup(CauseNormalClearing)
	}
	return nil
}

// voicemailFull reports a mailbox over its quota: vm_disk_quota (call variable, seconds of
// messages) against the sum of the message lengths.
func (c *Core) voicemailFull(ctx context.Context, ch *Channel, vm *config.Voicemail) bool {
	quota, err := strconv.Atoi(strings.TrimSpace(ch.Var("vm_disk_quota")))
	if err != nil || quota <= 0 {
		return false
	}
	rows, err := c.sqlRows(ctx, `SELECT coalesce(sum(message_length), 0)::text AS n FROM v_voicemail_messages WHERE voicemail_uuid = $1::uuid`, vm.UUID)
	if err != nil || len(rows) == 0 {
		return false
	}
	n, _ := strconv.Atoi(strings.SplitN(rows[0]["n"], ".", 2)[0])
	return n >= quota
}

// vmFormatSeconds is format_seconds.lua: h:mm:ss (m:ss under an hour).
func vmFormatSeconds(n int) string {
	if n >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", n/3600, n%3600/60, n%60)
	}
	return fmt.Sprintf("%d:%02d", n/60, n%60)
}

// voicemailAfterMessage is what follows a message left in the mailbox: the copies for the
// mailbox's destinations and the mails. The caller does not wait for it.
func (c *Core) voicemailAfterMessage(ch *Channel, vm *config.Voicemail, msgID, path, cidName, cidNum string, secs int) {
	vars := ch.Vars()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				c.log.Error("voicemail follow-up panicked", "panic", r)
			}
		}()
		boxes := []*config.Voicemail{vm}
		msgs := []string{msgID}
		files := []string{path}
		if rows, err := c.sqlRows(ctx, `SELECT voicemail_uuid_copy::text AS u FROM v_voicemail_destinations WHERE voicemail_uuid = $1::uuid`, vm.UUID); err == nil {
			for _, r := range rows {
				cp := c.voicemailByUUID(r["u"])
				if cp == nil || cp.UUID == vm.UUID {
					continue
				}
				id := sip.NewUUID()
				dst := filepath.Join(c.voicemailDir(cp.Domain, cp.ID), "msg_"+id+".wav")
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil || copyFile(path, dst) != nil {
					c.log.Warn("voicemail: copy failed", "to", cp.ID+"@"+cp.Domain)
					continue
				}
				if _, err := c.sqlExec(ctx, `INSERT INTO v_voicemail_messages (voicemail_message_uuid, domain_uuid, voicemail_uuid, created_epoch, caller_id_name, caller_id_number, message_length, message_status, message_priority, insert_date)
					SELECT $1::uuid, domain_uuid, $2::uuid, $3::numeric, $4, $5, $6::numeric, '', 'normal', now() FROM v_voicemails WHERE voicemail_uuid = $2::uuid`,
					id, cp.UUID, strconv.FormatInt(time.Now().Unix(), 10), cidName, cidNum, strconv.Itoa(secs)); err != nil {
					c.log.Warn("voicemail: copy not recorded", "err", err)
					_ = os.Remove(dst)
					continue
				}
				boxes, msgs, files = append(boxes, cp), append(msgs, id), append(files, dst)
				c.recountMWI(ctx, cp)
			}
		}
		for i, box := range boxes {
			if err := c.voicemailMail(ctx, vars, box, msgs[i], files[i], cidName, cidNum, secs); err != nil {
				c.log.Warn("voicemail: mail not queued", "box", box.ID+"@"+box.Domain, "err", err)
			}
		}
	}()
}

func (c *Core) voicemailByUUID(u string) *config.Voicemail {
	for _, v := range c.config().Features.Voicemails {
		if strings.EqualFold(v.UUID, u) {
			return v
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// voicemailMail queues the mail of one message (send_email.lua).
func (c *Core) voicemailMail(ctx context.Context, vars map[string]string, vm *config.Voicemail, msgID, file, cidName, cidNum string, secs int) error {
	rows, err := c.sqlRows(ctx, `SELECT coalesce(voicemail_mail_to, '') AS mail_to, coalesce(voicemail_file, '') AS file, coalesce(voicemail_local_after_email::text, 'true') AS keep,
		coalesce(voicemail_description, '') AS descr, coalesce(voicemail_transcription_enabled::text, '') AS transcribe, domain_uuid::text AS domain_uuid
		FROM v_voicemails WHERE voicemail_uuid = $1::uuid`, vm.UUID)
	if err != nil || len(rows) == 0 {
		if err == nil {
			err = fmt.Errorf("mailbox not found")
		}
		return err
	}
	r := rows[0]
	if len(r["mail_to"]) <= 2 {
		return nil
	}
	domainUUID := r["domain_uuid"]
	mode := disaDefault(r["file"], "listen")
	lang := strings.ToLower(disaDefault(vars["default_language"], "en") + "-" + disaDefault(vars["default_dialect"], "us"))
	sub := "default"
	if c.scriptSetting(ctx, domainUUID, "voicemail", "transcribe_enabled", "boolean") == "true" && r["transcribe"] == "true" {
		sub = "transcription"
	}
	nameFmt := vm.ID
	if c.scriptSetting(ctx, domainUUID, "voicemail", "display_domain_name", "boolean") == "true" {
		nameFmt = vm.ID + "@" + vm.Domain
	}
	if d := r["descr"]; d != "" && d != vm.ID {
		nameFmt += " (" + d + ")"
	}
	newCount := 0
	for _, m := range c.voicemailMessages(ctx, vm) {
		if m.New {
			newCount++
		}
	}
	proto := disaDefault(c.scriptSetting(ctx, domainUUID, "domain", "http_protocol", "text"), "https")
	link := proto + "://" + vm.Domain + "/app/voicemails/voicemail_messages.php?action=autoplay&id=" + vm.UUID + "&uuid=" + msgID
	var message string
	switch mode {
	case "attach":
		message = "Attached"
	case "link":
		message = "<a href='" + proto + "://" + vm.Domain + "/app/voicemails/voicemail_messages.php?action=download&id=" + vm.ID + "&voicemail_uuid=" + vm.UUID + "&uuid=" + msgID + "'>Download Message</a>"
	default:
		message = "<a href='" + link + "'>Listen to Message</a>"
	}
	vals := map[string]string{
		"caller_id_name": cidName, "caller_id_number": cidNum, "message_date": time.Now().Format("Monday 02 Jan 2006 03:04:05 PM"),
		"message_duration": vmFormatSeconds(secs), "account": nameFmt, "voicemail_id": vm.ID, "voicemail_description": r["descr"],
		"voicemail_name_formatted": nameFmt, "domain_name": vm.Domain, "new_messages": strconv.Itoa(newCount),
		"sip_to_user": vm.ID, "dialed_user": vm.ID, "message": message, "message_text": "",
		"origination_callee_id_name": vars["origination_callee_id_name"],
	}
	subject, body, ok := c.emailTemplate(ctx, domainUUID, lang, "voicemail", sub)
	if ok {
		subject, body = strings.TrimSpace(fillTemplate(subject, vals)), strings.TrimSpace(fillTemplate(body, vals))
	} else {
		subject = "Voicemail " + cidName + " <" + cidNum + "> " + vmFormatSeconds(secs)
		body = "<html><body>"
		if cidName != "" && cidName != cidNum {
			body += cidName + "<br>"
		}
		body += cidNum + "<br>" + vals["message_date"] + "<br><br>" + message + "</body></html>"
	}
	m := EmailMessage{DomainUUID: domainUUID, Type: "voicemail", CallUUID: msgID, To: r["mail_to"],
		Subject: mimeSubject(subject), Body: body, DeleteAfter: r["keep"] == "false"}
	if mode == "attach" {
		m.Attachment = file
	}
	return c.QueueEmail(ctx, m)
}
