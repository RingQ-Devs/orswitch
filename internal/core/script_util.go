package core

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ringq-devs/orswitch/internal/sip"
)

// Helpers the ported FusionPBX scripts (script_disa.go, script_wakeup.go, script_recordings.go,
// script_apps.go ...) share: the session calls the Lua scripts made (playAndGetDigits, streamFile,
// say), the hangup hook, domain and default settings, and the email queue.

// Header is a call variable, or, for a script run by api_hangup_hook, a header of the hangup event
// (env:getHeader).
func (s *Script) Header(name string) string {
	if s.Env != nil {
		return s.Env[name]
	}
	return s.Var(name)
}

// promptFor maps the FreeSWITCH phrase macros the scripts used to the built-in prompts; every other
// name is a file in the sounds folders.
func (s *Script) promptFor(name string) string {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, "phrase:") {
		return name
	}
	macro := strings.SplitN(strings.TrimPrefix(name, "phrase:"), ":", 2)[0]
	switch macro {
	case "voicemail_enter_pass", "pin_number_start":
		return "prompts/pin-enter.wav"
	case "voicemail_fail_auth":
		return "prompts/pin-invalid.wav"
	}
	return "" // no recording of the macro: the step is silent
}

// Stream is session:streamFile: plays a file (a missing one is skipped, like a prompt nobody
// uploaded) until the end or a hangup.
func (s *Script) Stream(name string) {
	if !s.Ready() {
		return
	}
	_, _ = s.Core.playSound(s.Ctx, s.Ch, s.promptFor(name), false)
}

// PlayAndGetDigits is session:playAndGetDigits(min, max, tries, timeout, terminators, prompt,
// invalid, regex): the digits, or "" when every try failed. timeout is both the wait for the first
// digit and between digits.
func (s *Script) PlayAndGetDigits(minD, maxD, tries int, timeout time.Duration, terms, prompt, invalid, regex string) string {
	if !s.Ready() {
		return ""
	}
	var re *regexp.Regexp
	if regex != "" && regex != ".*" {
		if r, err := regexp.Compile(regex); err == nil {
			re = r
		}
	}
	if terms == "none" {
		terms = ""
	}
	if tries < 1 {
		tries = 1
	}
	for try := 0; try < tries && s.Ready(); try++ {
		s.Ch.FlushDTMF()
		first, err := s.Core.playSound(s.Ctx, s.Ch, s.promptFor(prompt), true)
		if err != nil {
			return ""
		}
		digits, err := collectDigits(s.Ctx, s.Ch, first, maxD, terms, timeout, timeout)
		if err != nil {
			return ""
		}
		if len(digits) >= minD && (len(digits) > 0 || minD == 0) && (re == nil || re.MatchString(digits)) {
			return digits
		}
		if invalid != "" && try < tries-1 {
			s.Stream(invalid)
		}
	}
	return ""
}

// GetDigitsLoose is session:getDigits(max, terminators, timeout): digits without a prompt.
func (s *Script) GetDigitsLoose(maxD int, terms string, timeout time.Duration) string {
	if !s.Ready() {
		return ""
	}
	digits, _ := collectDigits(s.Ctx, s.Ch, 0, maxD, terms, timeout, timeout)
	return digits
}

// SayDigits is session:say(text, "en", "number", "iterated"): each digit is spoken on its own.
func (s *Script) SayDigits(text string) {
	for _, d := range text {
		if !s.Ready() {
			return
		}
		if d >= '0' && d <= '9' {
			if f := s.Core.prompt(s.Ch, "num-"+string(d)); f != "" {
				_, _ = s.Core.play(s.Ctx, s.Ch, f, false)
			}
		}
	}
}

// Transfer is session:transfer(destination, "XML", context).
func (s *Script) Transfer(dest, context string) error {
	return s.Execute("transfer", dest+" XML "+context)
}

// DomainUUID is the domain_uuid of the call, or the one of domain (looked up by name).
func (s *Script) DomainUUID(domain string) string {
	if v := s.Var("domain_uuid"); v != "" && (domain == "" || domain == s.Var("domain_name")) {
		return v
	}
	if v := s.Header("domain_uuid"); v != "" && domain == "" {
		return v
	}
	return s.Core.domainUUID(s.Ctx, domain)
}

// domainUUID looks a domain up by name ("" if unknown or without a database).
func (c *Core) domainUUID(ctx context.Context, domain string) string {
	if domain == "" {
		return ""
	}
	rows, err := c.sqlRows(ctx, "SELECT domain_uuid::text AS u FROM v_domains WHERE domain_name = $1 LIMIT 1", domain)
	if err != nil || len(rows) == 0 {
		return ""
	}
	return rows[0]["u"]
}

// scriptSetting is settings:get(category, subcategory, name): the domain's setting, else the
// default setting ("" when neither is enabled).
func (c *Core) scriptSetting(ctx context.Context, domainUUID, category, sub, name string) string {
	if domainUUID != "" {
		if rows, err := c.sqlRows(ctx, `SELECT domain_setting_value AS v FROM v_domain_settings WHERE domain_uuid = $1::uuid
			AND domain_setting_enabled::text IN ('true', 't') AND domain_setting_category = $2 AND domain_setting_subcategory = $3
			AND domain_setting_name = $4 AND domain_setting_value IS NOT NULL ORDER BY domain_setting_category LIMIT 1`,
			domainUUID, category, sub, name); err == nil && len(rows) > 0 {
			return rows[0]["v"]
		}
	}
	if rows, err := c.sqlRows(ctx, `SELECT default_setting_value AS v FROM v_default_settings WHERE default_setting_enabled::text IN ('true', 't')
		AND default_setting_category = $1 AND default_setting_subcategory = $2 AND default_setting_name = $3
		AND default_setting_value IS NOT NULL ORDER BY default_setting_category LIMIT 1`, category, sub, name); err == nil && len(rows) > 0 {
		return rows[0]["v"]
	}
	return ""
}

// ---------------------------------------------------------------------------
// api_hangup_hook

// runHangupHook runs the call's api_hangup_hook when it ends, the way FreeSWITCH does: an API
// command, usually "lua <script> [args]", with the call's variables as the event the script reads
// (Script.Env, like the Lua env:getHeader). The FusionPBX fax scripts and "app.lua hangup" use it.
func (c *Core) runHangupHook(ch *Channel) {
	hook := strings.TrimSpace(ch.Var("api_hangup_hook"))
	if hook == "" {
		return
	}
	// The log says which hook ran (its command word and the script), never the expanded text: the arguments carry
	// whatever channel variables the template pulled in.
	label := hookLabel(hook)
	env := ch.Vars()
	env["uuid"] = ch.UUID
	env["Unique-ID"] = ch.UUID
	env["Caller-Caller-ID-Name"] = env["caller_id_name"]
	env["Caller-Caller-ID-Number"] = env["caller_id_number"]
	hook = c.expand(hook, func(name string) string { return env[name] })
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.log.Error("hangup hook panicked", "hook", label, "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		f := strings.Fields(hook)
		if len(f) >= 2 && (f[0] == "lua" || f[0] == "luarun") && c.canRunScript(f[1], f[2:]) {
			if _, _, err := c.runScriptEnv(ctx, nil, f[1], f[2:], env); err != nil {
				c.log.Warn("hangup hook failed", "hook", label, "err", err)
			}
			return
		}
		c.API(hook)
	}()
}

// ---------------------------------------------------------------------------
// Email: the FusionPBX email queue

// EmailMessage is one mail for the queue (resources/functions/send_mail.lua).
type EmailMessage struct {
	DomainUUID string
	Type       string // X-FusionPBX-Email-Type (info, voicemail, missed, ...)
	CallUUID   string // the mail's uuid (the call's, else a new one)
	From       string // "" = the email/smtp_from setting
	To         string
	Subject    string
	Body       string
	Attachment string // path of a file to attach ("" none)
	// DeleteAfter: the local copy goes once the mail is sent (voicemail_local_after_email=false).
	DeleteAfter bool
}

var emailMime = map[string]string{
	"jpg": "image/jpeg", "peg": "image/jpeg", "gif": "image/gif", "png": "image/png", "pdf": "application/pdf",
	"tif": "image/tiff", "iff": "image/tiff", "mp3": "audio/mpeg", "wav": "audio/x-wav", "pus": "audio/opus", "ogg": "audio/ogg",
}

// QueueEmail adds a mail to v_email_queue as send_mail.lua does (FusionPBX's email queue service
// sends it; the switch itself does not talk SMTP).
func (c *Core) QueueEmail(ctx context.Context, m EmailMessage) error {
	if m.To == "" {
		return fmt.Errorf("email: no recipient")
	}
	from := m.From
	if from == "" {
		from = c.scriptSetting(ctx, m.DomainUUID, "email", "smtp_from", "text")
		name := c.scriptSetting(ctx, m.DomainUUID, "email", "smtp_from_name", "text")
		switch {
		case from == "":
			from = m.To
		case name != "":
			from = name + "<" + from + ">"
		}
	}
	uuid := m.CallUUID
	if uuid == "" || uuid == "na" {
		uuid = sip.NewUUID()
	}
	action := ""
	if m.DeleteAfter {
		action = "delete"
	}
	host, _ := os.Hostname()
	queueUUID := sip.NewUUID()
	var dom any
	if m.DomainUUID != "" {
		dom = m.DomainUUID
	}
	if _, err := c.sqlExec(ctx, `INSERT INTO v_email_queue (email_queue_uuid, domain_uuid, hostname, email_date, email_from, email_to,
		email_subject, email_body, email_status, email_uuid, email_action_after, insert_date)
		VALUES ($1::uuid, $2::uuid, $3, now(), $4, $5, $6, $7, 'waiting', $8::uuid, $9, now())`,
		queueUUID, dom, host, from, m.To, m.Subject, m.Body, uuid, action); err != nil {
		return err
	}
	if m.Attachment != "" {
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(m.Attachment), "."))
		typ := ext
		if len(m.Attachment) >= 3 {
			typ = strings.ToLower(m.Attachment[len(m.Attachment)-3:])
		}
		mime := emailMime[typ]
		if mime == "" {
			mime = "binary/octet-stream"
		}
		if _, err := c.sqlExec(ctx, `INSERT INTO v_email_queue_attachments (email_queue_attachment_uuid, email_queue_uuid, domain_uuid,
			email_attachment_mime_type, email_attachment_type, email_attachment_path, email_attachment_name, email_attachment_base64, insert_date)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7, '', now())`,
			sip.NewUUID(), queueUUID, dom, mime, typ, filepath.Dir(m.Attachment), filepath.Base(m.Attachment)); err != nil {
			return err
		}
	}
	c.log.Info("email added to the queue", "to", m.To, "type", m.Type, "queue", queueUUID)
	return nil
}

// emailTemplate reads v_email_templates (the domain's before the global one).
func (c *Core) emailTemplate(ctx context.Context, domainUUID, language, category, subcategory string) (subject, body string, ok bool) {
	q := `SELECT template_subject AS s, template_body AS b FROM v_email_templates
		WHERE (domain_uuid = $1::uuid OR domain_uuid IS NULL) AND template_language = $2 AND template_category = $3
		AND template_enabled::text IN ('true', 't')`
	args := []any{domainUUID, language, category}
	if subcategory != "" {
		q += " AND template_subcategory = $4"
		args = append(args, subcategory)
	}
	q += " ORDER BY domain_uuid DESC NULLS LAST LIMIT 1"
	if domainUUID == "" {
		q = strings.Replace(q, "(domain_uuid = $1::uuid OR domain_uuid IS NULL)", "($1::text = '' OR domain_uuid IS NULL)", 1)
	}
	rows, err := c.sqlRows(ctx, q, args...)
	if err != nil || len(rows) == 0 {
		return "", "", false
	}
	return rows[0]["s"], rows[0]["b"], true
}

// fillTemplate replaces ${name} by the values (templates of the Lua scripts).
func fillTemplate(text string, vals map[string]string) string {
	for k, v := range vals {
		text = strings.ReplaceAll(text, "${"+k+"}", v)
	}
	return text
}

// htmlQuote is what the Lua scripts did to caller names before a template: ' and " as entities.
func htmlQuote(s string) string {
	s = strings.ReplaceAll(s, "'", "&#39;")
	return strings.ReplaceAll(s, `"`, "&#34;")
}

// mimeSubject encodes a mail subject (=?utf-8?B?...?=) like the Lua scripts did.
func mimeSubject(subject string) string {
	return "=?utf-8?B?" + b64(strings.TrimSpace(subject)) + "?="
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
