# RingQ Core compatibility

What `ringq_core` (PHP + Node services) expects from its switch, and how ORswitch
covers it. Produced by scanning Core's source (`origin/main` at 4019645) for every event
socket (ESL) command, event and dialplan application it uses, then checking each against
this switch and exercising the new pieces with Core's own `modesl` client.

## Summary

| Area | Core uses | Provided | Missing |
|---|---|---|---|
| ESL API commands | 38 | all (including `uuid_audio_stream`) | none |
| Dialplan applications | 20 | 20 | none |
| Events Core handles | 31 | all, `avmd::beep` included (see below) | `API` |
| Event headers | | `Channel-Call-UUID` added | |
| `uuid_dump <uuid> JSON` | 24 call sites | now returns flat JSON | |

`uuid_exist` (no such FreeSWITCH command, a typo in Core) and the placeholder `x` found by the
scan are not real commands.

## Provided by this switch

* **Commands:** `sched_api`, `sched_transfer`, `sched_hangup`, `sched_del`, `uuid_break`,
  `uuid_audio` (accepted, no level change), `reloadacl`, `json`, `uuid_buglist`, and
  `uuid_dump <uuid> JSON`.
* **Events:** `CHANNEL_STATE`, `CHANNEL_PARK`/`CHANNEL_UNPARK`, `CHANNEL_OUTGOING`,
  `RECORD_START`/`RECORD_STOP`, `PRESENCE_IN` (with `event_count`).
* **Registrations:** a phone that registers with the extension's `number_alias` (the web dialer
  does) is reported by that alias in events, `show registrations` and
  `sofia status profile internal reg` (FreeSWITCH text layout, parsed by Core).
  Core flushes any registration whose reported username is only digits.
* **SIP over WebSocket (wss, port 7443) with WebRTC media**, G.711 only.

## Known gaps

1. Phones that register with a bare extension number are flushed by Core by design; use the
   alias login (as on the RingQ servers).
2. Opus (needs a C library; the switch is pure Go) and codecs other than G.711 and G.722.
3. SRTP covers SDES key exchange for plain SIP legs (see "SRTP and SIP over TLS" below); not done: SRTCP,
   several key lines per offer, and DTLS-SRTP for desk phones (the WebRTC gateway has its own).
4. The `callcenter::info` events and member list follow mod_callcenter's shape so that RingQ's wallboard counts
   correctly (see "Call centre data for the wallboard"). `members-count` is not sent (Core does not use it).

The Lua scripts, `uuid_audio_stream`, `mod_ringq` (push wake) and answering-machine detection that earlier
versions of this list named as missing are provided: see below.

## Call centre data for the wallboard

RingQ Core's wallboard (`node/queue.js`, `node/server.js`) reads `callcenter::info` events and
`callcenter_config member list|agent list|tier list|queue list`. What it relies on, and what the switch sends:

- Every caller event (`member-queue-start`, `agent-offering`, `bridge-agent-start/end/fail`, `member-queue-end`) carries
  the caller's channel headers (`Channel-Call-State`, `Channel-Call-UUID`, `Unique-ID`, `variable_*` such as
  `variable_domain_name`, `variable_cc_queue`). Core adds a caller to its call board only while `Channel-Call-State`
  is ACTIVE, which is why the queue answers the caller before it joins, as mod_callcenter does.
- `agent-offering` is sent for every agent offered a call, `bridge-agent-fail` for every offer that did not end in a
  call (`NO_ANSWER`, `LOSE_RACE`, `ORIGINATOR_CANCEL` when the caller left while the phone rang, ...), with
  `CC-Member-Session-UUID` (Core's "Missed" count for an agent is keyed on it).
- `member-queue-end`: `CC-Cause` is `Terminated` (answered) or `Cancel`; `CC-Cancel-Reason` is only on a cancel and is
  `BREAK_OUT` (the caller hung up), `TIMEOUT`, `NO_AGENT_TIMEOUT` or `EXIT_WITH_KEY`.
- A caller who left without an agent stays in `member list` as `Abandoned` with `abandoned_epoch` for
  `discard-abandoned-after` seconds (60 by default); Core reads an abandoned call's waiting time from that row.
- `bridge-agent-start/end` carry `CC-Agent-UUID` (the agent's own channel), `CC-Agent-Called-Time`,
  `CC-Agent-Answered-Time` and `CC-Bridge-Terminated-Time`.

Tests: `TestQueueWallboardData`, `TestQueueAgentNoAnswerEvent`.

## Answering machine detection (mod_avmd)

`avmd <uuid> start|stop`, the dialplan applications `avmd`, `avmd_start`, `avmd_stop` and `module_exists mod_avmd`
(`internal/core/avmd.go`). It listens to the audio the far end sends and fires `CUSTOM avmd::beep` (`Beep-Status:
DETECTED`, `Frequency`, `Frequency-variance`, `Detection-time`, the channel's headers) when one steady tone between
300 and 3400 Hz holds for `avmd_min_tone_ms` (default 120 ms), and sets `avmd_detect=TRUE`. `avmd::start` and
`avmd::stop` mark its life; after the first beep it stops unless `avmd_repeat=true`. Voice, DTMF and ringback (two
tones) do not count. Tests: `TestAVMD*`.

## G.722

Pure Go, bit-exact with the reference codec (`internal/media/g722.go`; the tests compare with libspandsp's output).
Opt in with `codec-prefs` (`PCMU,PCMA,G722`). Two G.722 legs are bridged without touching the audio; G.722 to G.711 is
converted per stream. The switch's own audio (IVR, voicemail, tones) is sent as G.722 on a G.722 leg (8 kHz source).

## SRTP and SIP over TLS

- **SIP over TLS:** `sip-tls-port` in `<web>` (for example 5061) opens a TLS listener with the portal's certificate;
  registrations and calls over it are `tls` transport (`Via: SIP/2.0/TLS`, `Contact ...;transport=tls`). A gateway can
  set `transport="tls"` (port 5061 by default; the trunk's certificate chain and name are verified unless `sip-tls-verify=false`).
- **SRTP (SDES, RFC 4568):** a caller that offers `RTP/SAVP` with an `a=crypto` line (`AES_CM_128_HMAC_SHA1_80` or
  `_32`) is answered with SRTP and the switch's own key (setting `srtp`, `optional` by default, `off` refuses). Each
  leg of a bridged call has its own keys, so an SRTP phone can talk to a plain RTP trunk. The legs the switch starts are
  plain unless the dialplan sets `rtp_secure_media=true` (or `mandatory`, or a suite name): they are then offered
  SRTP and a plain answer ends the leg. SRTCP is neither generated nor relayed.
  Tests: `TestSRTP*`, `TestTransactionsOverTLS`.

## How to re-run the scan

The scan reads Core's `node/*.js` and `app/**/*.php`, extracts `conn.api(...)`, `bgapi`,
`promisifiedApi(...)`, `fs_cli -x`, event names and `<action application=...>`, and compares them
with `or_cli -x help` and `or_cli -x "show applications"` of a running switch.

## Ring push (mod_ringq equivalent)

The RingQ backend wakes a phone's app with `node/ringPush.js` and the FreeSWITCH module `mod_ringq`.
ORswitch does the module's part itself (`internal/core/ringpush.go`):

- Enabled per domain by the RingQ setting `ringq / ring_push_mode = wake` (v_domain_settings, else
  v_default_settings); `module_exists mod_ringq` answers true so ringPush.js takes over from the old push code.
- Every dial of `user/<ext>[@<domain>]` (direct, queue, IVR, ring group, forward, transfer) fires
  `CUSTOM ringq::ring` (`ringq-ext`, `ringq-domain`, `ringq-ring-id`, `ringq-mode`, `ringq-a-uuid`,
  `ringq-registered`, `ringq-caller-number`, `ringq-caller-name`, `ringq-queue`).
- ringPush.js answers with `hash insert/ringq_ring/<ring id>/<phones pushed>/<iPhones pushed>`.
- Not in a forked dial and phones were pushed: the leg waits for the RingQ mobile apps (User-Agent
  `RingQ iOS - ...` / `RingQ android - ...`) to register, up to `ringq_ring_wake_wait_ms` (15 s; 3 s with a
  desk phone online, 8 s for the rest once one pushed phone is up).
- The INVITE carries the ring id as its SIP Call-ID (`sip_invite_call_id`), and the leg variables
  `ringq_ring_id` / `ringq_ring_ext`, so the app matches it to the push.
- When the originate ends: `CUSTOM ringq::ring_end` (`ringq-cause`, `ringq-answered`, `ringq-registered`).

Tests: `TestRingPushWake`, `TestRingPushNothingPushed`.

## uuid_openai_audio_stream (mod_openai_audio_stream equivalent)

`internal/core/openai_stream.go` replaces the FreeSWITCH module the RingQ AI dialplans call
(`${uuid_openai_audio_stream ${uuid} start wss://... mono 24k}`; `${command args}` now runs any API
command in a dialplan, as FreeSWITCH does). Commands: `start <wss-url> <mono|mixed|stereo> <8k|16k|24k>`,
`stop [base64 json]`, `pause`, `resume`, `send_json <base64 json>`. Behaviour as the module:

- connects with `Authorization: Bearer STREAM_OPENAI_API_KEY` (or the JSON headers in
  `STREAM_EXTRA_HEADERS`); other variables: `STREAM_NO_RECONNECT`, `STREAM_HEART_BEAT`,
  `STREAM_BUFFER_SIZE`, `STREAM_TLS_*`
- on open sends `session.update` (`STREAM_OPENAI_VOICE`, `_LANGUAGE`, `_NOISE`, `_INSTRUCTIONS`) and a
  `response.create` with `STREAM_OPENAI_GREETING`
- the caller's audio goes up as `input_audio_buffer.append`, `response.output_audio.delta` is played to the
  caller (24 kHz in), `input_audio_buffer.speech_started` drops what is still to be played
- a keyword of `STREAM_OPENAI_TRANSFER_MAP` ("sales:6000,support:6001") in the caller's transcript
  transfers the call after the AI has spoken (what transfer_ai_call.lua did)
- events `mod_openai_audio_stream::{connect,disconnect,error,json,openai_speech_start,openai_speech_stop}`
- while the stream runs it owns the call's audio: `playback silence_stream://-1` only keeps the call up.
  Stereo and mixed starts are taps (both parties). Not done: debug WAV files (Core sets no variable for them). `STREAM_DISABLE_AUDIOFILES=true`, which RingQ's AI dialplans set, is honoured by `uuid_audio_stream`: streamAudio messages are then not written to files and send no `::play` event.

## RingQ scripts (the Lua replacement)

The Lua scripts of the backend are Go functions registered by name (`scripts.go`, `RegisterScript`), so the
dialplans (`lua <script> args`), `${lua(script args)}`, the API (`lua`, `luarun`) and
`uuid_transfer <uuid> lua:<script> inline` keep working unchanged. A script gets a `Script` with the
session-like helpers (Var, SetVar, Execute, Hangup, Answer, Sleep, API) and, for the backend's data, a Redis
client (`redis-addr` setting, default the usual socket) and the database reader.

| Script | What it does here |
|---|---|
| `ringq_call_control` | resolve (per-profile forwarding from q_call_behaviors + Redis `<ext>+<domain>`), screen (On Busy, call screening with `group_confirm`), failover (Call Relay, On Busy, No Answer) |
| `ringq_att_voicemail` | attended transfer: the agent hears the target's voicemail, then is bridged back (needs `/usr/share/freeswitch/scripts/ringq_att_voicemail.lua` to exist, the backend checks it) |
| `ringq_desk_park` | shared parking from desk phones (Redis `<domain>:parked`, BLF lamps `*591..*595`, `ringq::park`) |
| `ringq_desk_aux` | agent status keys (`*26<id>`) |
| `ringq_blf_pickup` | BLF pickup (`*8<sip user>`) |
| `streamfile`, `ivr_ai2`, `call_forward`, `pin_number` | FusionPBX scripts the RingQ dialplans use |

Not ported: FusionPBX `follow_me.lua` / `app.lua follow_me`, `intercept` display updates, the `uuid_audio_stream`
(mod_audio_stream) command for the older AI External extensions.

## Differences from FreeSWITCH on purpose (security)

- `${user_data <user>@<domain> param password}` (also `a1-hash`, `a1-md5` and any `*-password` parameter) returns an empty
  string and logs a warning. FreeSWITCH hands the value out; here it would end up in channel variables, events (every
  event socket client sees `variable_*`), call records and logs. Nothing in RingQ's dialplans or Core uses it.
- `STREAM_TLS_CA_FILE=NONE` (uuid_openai_audio_stream) does not switch certificate checking off; the server certificate
  is verified (system roots, or the CA file when one is given). `STREAM_TLS_DISABLE_HOSTNAME_VALIDATION` still verifies
  the certificate chain and only skips the name check.
- The desk-phone status key script (`ringq_desk_aux`) checks the web site's certificate like any client.
- TLS to a SIP trunk verifies the certificate unless `sip-tls-verify=false`.
- The log names scripts, applications and API commands only when the switch knows them, and never prints the digits a
  caller keyed in at a mailbox prompt.
