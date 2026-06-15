package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidandcat/ccc/internal/mail"
	"github.com/kidandcat/ccc/internal/registry"
	"github.com/kidandcat/ccc/internal/scheduler"
)

func rpc(method, params string, id string) rpcRequest {
	r := rpcRequest{JSONRPC: "2.0", Method: method}
	if params != "" {
		r.Params = json.RawMessage(params)
	}
	if id != "" {
		r.ID = json.RawMessage(id)
	}
	return r
}

func TestHandleRPCInitializeEchoesVersion(t *testing.T) {
	resp, reply := handleRPC(rpc("initialize", `{"protocolVersion":"2025-06-18"}`, "1"), "/tmp")
	if !reply {
		t.Fatal("initialize must produce a reply")
	}
	m, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatalf("result not a map: %T", resp.Result)
	}
	if m["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v, want echoed 2025-06-18", m["protocolVersion"])
	}
	si := m["serverInfo"].(map[string]interface{})
	if si["name"] != "ccc-secretary" {
		t.Fatalf("serverInfo.name = %v", si["name"])
	}
}

func TestHandleRPCInitializeDefaultsVersion(t *testing.T) {
	resp, _ := handleRPC(rpc("initialize", `{}`, "1"), "/tmp")
	m := resp.Result.(map[string]interface{})
	if m["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("default protocolVersion = %v, want %s", m["protocolVersion"], mcpProtocolVersion)
	}
}

func TestHandleRPCToolsList(t *testing.T) {
	resp, reply := handleRPC(rpc("tools/list", "", "2"), "/tmp")
	if !reply {
		t.Fatal("tools/list must reply")
	}
	tools := resp.Result.(map[string]interface{})["tools"].([]map[string]interface{})
	got := map[string]bool{}
	for _, tdef := range tools {
		got[tdef["name"].(string)] = true
	}
	for _, want := range []string{"send", "list_agents", "get_agent", "update_self"} {
		if !got[want] {
			t.Fatalf("tools/list missing %q (got %v)", want, got)
		}
	}
}

func TestHandleRPCNotificationNoReply(t *testing.T) {
	_, reply := handleRPC(rpc("notifications/initialized", "", ""), "/tmp")
	if reply {
		t.Fatal("notifications/initialized must not produce a reply")
	}
}

func TestHandleRPCUnknownMethod(t *testing.T) {
	// With id -> JSON-RPC error.
	resp, reply := handleRPC(rpc("bogus/method", "", "9"), "/tmp")
	if !reply || resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("unknown method: reply=%v err=%+v", reply, resp.Error)
	}
	// Without id (notification) -> silently ignored.
	if _, reply := handleRPC(rpc("bogus/notification", "", ""), "/tmp"); reply {
		t.Fatal("unknown notification must be ignored")
	}
}

func TestBuildDirectoryMergesCardsAndSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	registry.SetCard(registry.Card{Agent: "backend", Description: "api", Areas: []string{"db"}})
	registry.SetCard(registry.Card{Agent: "ghost", Description: "card-only, no session"})

	cfg := &Config{Sessions: map[string]*SessionInfo{
		"backend":  {TopicID: 10, Path: "/p/backend", Host: ""},
		"frontend": {TopicID: 11, Path: "/p/frontend"},
		"old":      {TopicID: 12, Path: "/p/old", Deleted: true},
	}}

	dir := buildDirectory(cfg)
	byName := map[string]AgentInfo{}
	for _, a := range dir {
		byName[a.Name] = a
	}
	if _, ok := byName["old"]; ok {
		t.Fatal("deleted session must be excluded")
	}
	if b := byName["backend"]; b.Description != "api" || b.TopicID != 10 || len(b.Areas) != 1 {
		t.Fatalf("backend not merged: %+v", b)
	}
	if f := byName["frontend"]; f.Description != "" || f.TopicID != 11 {
		t.Fatalf("frontend should have mechanical only: %+v", f)
	}
	if g, ok := byName["ghost"]; !ok || g.Description != "card-only, no session" {
		t.Fatalf("card-only agent missing: %+v ok=%v", g, ok)
	}
}

func TestResolveCaller(t *testing.T) {
	cfg := &Config{Sessions: map[string]*SessionInfo{
		"backend":     {Path: "/home/x/proj/backend"},                   // local (host "")
		"dead":        {Path: "/home/x/proj/dead", Deleted: true},       // local, deleted
		"XPS:backend": {Path: "/home/x/proj/backend", Host: "XPS"},      // remote, SAME path
	}}
	// Local caller (host "") resolves to the local session, not the remote one
	// sharing the same path.
	if got := resolveCaller(cfg, "", "/home/x/proj/backend"); got != "backend" {
		t.Fatalf("local resolveCaller = %q, want backend", got)
	}
	if got := resolveCaller(cfg, "", "/home/x/proj/backend/"); got != "backend" {
		t.Fatalf("local trailing slash = %q, want backend", got)
	}
	// Remote caller with the same path resolves to the host-matched session.
	if got := resolveCaller(cfg, "XPS", "/home/x/proj/backend"); got != "XPS:backend" {
		t.Fatalf("remote resolveCaller = %q, want XPS:backend", got)
	}
	// Unknown host for that path does not match.
	if got := resolveCaller(cfg, "dell17", "/home/x/proj/backend"); got != "" {
		t.Fatalf("unknown host = %q, want empty", got)
	}
	if got := resolveCaller(cfg, "", "/home/x/proj/other"); got != "" {
		t.Fatalf("unknown path = %q, want empty", got)
	}
	if got := resolveCaller(cfg, "", "/home/x/proj/dead"); got != "" {
		t.Fatalf("deleted = %q, want empty", got)
	}
}

func TestRelayProcessBadRequest(t *testing.T) {
	out := relayProcess([]byte("{not json"))
	var resp APIResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("relay did not emit valid JSON: %v", err)
	}
	if resp.OK || !strings.Contains(resp.Error, "bad request") {
		t.Fatalf("expected bad-request error response, got %+v", resp)
	}
}

func TestGetAgentInfoCardOnlyAndMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	registry.SetCard(registry.Card{Agent: "solo", Description: "no session"})
	cfg := &Config{Sessions: map[string]*SessionInfo{}}

	info, ok := getAgentInfo(cfg, "solo", false)
	if !ok || info.Description != "no session" {
		t.Fatalf("card-only get failed: %+v ok=%v", info, ok)
	}
	if _, ok := getAgentInfo(cfg, "nobody", false); ok {
		t.Fatal("expected not-found for unknown agent")
	}
}

// TestSocketAgentCommandsEndToEnd drives the real socket dispatch in-process
// (via net.Pipe) to cover caller-id resolution -> registry write -> directory
// list, the same data path the mcp-secretary shim uses over the Unix socket.
func TestSocketAgentCommandsEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	backendCwd := filepath.Join(home, "proj", "backend")
	cfg := &Config{Sessions: map[string]*SessionInfo{
		"backend": {TopicID: 5, Path: backendCwd},
	}}
	if err := saveConfig(cfg); err != nil { // handleSocketConnection reloads from disk
		t.Fatal(err)
	}

	cli, srv := net.Pipe()
	go handleSocketConnection(srv, cfg)
	defer cli.Close()
	enc := json.NewEncoder(cli)
	dec := json.NewDecoder(cli)

	// 1) update_self: identity resolved from cwd, card persisted.
	if err := enc.Encode(APIRequest{Cmd: "agent.update_self", Cwd: backendCwd,
		Payload: json.RawMessage(`{"description":"owns api","areas":["db"]}`)}); err != nil {
		t.Fatal(err)
	}
	var r1 APIResponse
	if err := dec.Decode(&r1); err != nil {
		t.Fatal(err)
	}
	if !r1.OK {
		t.Fatalf("update_self failed: %s", r1.Error)
	}
	var card registry.Card
	json.Unmarshal(r1.Result, &card)
	if card.Agent != "backend" || card.Description != "owns api" {
		t.Fatalf("stored card wrong: %+v", card)
	}

	// 2) update_self from an unrecognized cwd is rejected (no spoofing).
	enc.Encode(APIRequest{Cmd: "agent.update_self", Cwd: "/nowhere", Payload: json.RawMessage(`{}`)})
	var r2 APIResponse
	dec.Decode(&r2)
	if r2.OK {
		t.Fatal("update_self from unknown cwd must fail")
	}

	// 3) list_agents shows backend with the merged card.
	enc.Encode(APIRequest{Cmd: "agent.list"})
	var r3 APIResponse
	dec.Decode(&r3)
	if !r3.OK {
		t.Fatalf("agent.list failed: %s", r3.Error)
	}
	var dir []AgentInfo
	json.Unmarshal(r3.Result, &dir)
	found := false
	for _, a := range dir {
		if a.Name == "backend" && a.Description == "owns api" && a.TopicID == 5 {
			found = true
		}
	}
	if !found {
		t.Fatalf("backend (with card) not in directory: %+v", dir)
	}
}

// TestMailSendStoresInSecretaryInbox covers the inbound mail path: any agent's
// send lands in the secretary's inbox with a trusted sender + ticket. Without a
// live secretary session the wake is best-effort, so the call still succeeds
// (with a warning) and the letter is durably stored.
func TestMailSendStoresInSecretaryInbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	backendCwd := filepath.Join(home, "proj", "backend")
	cfg := &Config{Sessions: map[string]*SessionInfo{
		"backend": {TopicID: 7, Path: backendCwd},
	}}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	cli, srv := net.Pipe()
	go handleSocketConnection(srv, cfg)
	defer cli.Close()
	enc := json.NewEncoder(cli)
	dec := json.NewDecoder(cli)

	// Missing subject is rejected.
	enc.Encode(APIRequest{Cmd: "mail.send", Cwd: backendCwd, Payload: json.RawMessage(`{"to":"devops"}`)})
	var rBad APIResponse
	dec.Decode(&rBad)
	if rBad.OK {
		t.Fatal("send without subject must fail")
	}

	// Unknown caller is rejected (no spoofing the sender).
	enc.Encode(APIRequest{Cmd: "mail.send", Cwd: "/nowhere", Payload: json.RawMessage(`{"to":"devops","subject":"x"}`)})
	var rSpoof APIResponse
	dec.Decode(&rSpoof)
	if rSpoof.OK {
		t.Fatal("send from unknown cwd must fail")
	}

	// Valid send: stored with trusted from + ticket.
	enc.Encode(APIRequest{Cmd: "mail.send", Cwd: backendCwd,
		Payload: json.RawMessage(`{"to":"devops","subject":"deploy v2","body":"please ship","reply_to":"backend","needs_confirmation":true}`)})
	var r APIResponse
	if err := dec.Decode(&r); err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Fatalf("send failed: %s", r.Error)
	}
	var res map[string]string
	json.Unmarshal(r.Result, &res)
	if res["ticket"] == "" {
		t.Fatalf("no ticket returned: %v", res)
	}

	// Letter physically in the secretary's inbox with the trusted sender.
	files, _ := os.ReadDir(mail.InboxDir(mail.SecretaryAgent))
	if len(files) != 1 {
		t.Fatalf("secretary inbox has %d files, want 1", len(files))
	}
	data, _ := os.ReadFile(filepath.Join(mail.InboxDir(mail.SecretaryAgent), files[0].Name()))
	var letter mail.Letter
	json.Unmarshal(data, &letter)
	if letter.From != "backend" || letter.To != "devops" || letter.Subject != "deploy v2" ||
		letter.ReplyTo != "backend" || !letter.NeedsConfirmation || letter.Ticket != res["ticket"] {
		t.Fatalf("stored letter wrong: %+v", letter)
	}
}

// readSecretaryJournal returns the secretary's journal entries.
func readSecretaryJournal(t *testing.T) []mail.JournalEntry {
	t.Helper()
	data, err := os.ReadFile(mail.JournalPath(mail.SecretaryAgent))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []mail.JournalEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e mail.JournalEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad journal line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func journalHas(entries []mail.JournalEntry, ticket, event string) *mail.JournalEntry {
	for i := range entries {
		if entries[i].Ticket == ticket && entries[i].Event == event {
			return &entries[i]
		}
	}
	return nil
}

// mailTestServer wires a net.Pipe to handleSocketConnection with the given
// sessions persisted to disk, and returns an encoder/decoder pair.
func mailTestServer(t *testing.T, sessions map[string]*SessionInfo) (*json.Encoder, *json.Decoder) {
	t.Helper()
	cfg := &Config{Sessions: sessions}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cli, srv := net.Pipe()
	go handleSocketConnection(srv, cfg)
	t.Cleanup(func() { cli.Close() })
	return json.NewEncoder(cli), json.NewDecoder(cli)
}

func TestMailDeliverSecretaryOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	enc, dec := mailTestServer(t, map[string]*SessionInfo{
		"backend":   {Path: filepath.Join(home, "backend")},
		"secretary": {Path: filepath.Join(home, "secretary")},
	})
	// A non-secretary caller cannot deliver.
	enc.Encode(APIRequest{Cmd: "mail.deliver", Cwd: filepath.Join(home, "backend"),
		Payload: json.RawMessage(`{"to":"devops","ticket":"T1"}`)})
	var r APIResponse
	dec.Decode(&r)
	if r.OK || !strings.Contains(r.Error, "secretary-only") {
		t.Fatalf("expected secretary-only rejection, got OK=%v err=%q", r.OK, r.Error)
	}
}

func TestMailDeliverUnknownRecipientLogsFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	enc, dec := mailTestServer(t, map[string]*SessionInfo{
		"secretary": {Path: filepath.Join(home, "secretary")},
	})
	// Recipient "ghost" has no session -> wakeAgent fails before any tmux call.
	// (body provided inline, exercising the override / old-contract path.)
	enc.Encode(APIRequest{Cmd: "mail.deliver", Cwd: filepath.Join(home, "secretary"),
		Payload: json.RawMessage(`{"to":"ghost","ticket":"T9","from":"backend","subject":"x","body":"hi"}`)})
	var r APIResponse
	dec.Decode(&r)
	if r.OK {
		t.Fatal("deliver to unknown recipient must fail")
	}
	if e := journalHas(readSecretaryJournal(t), "T9", "delivery_failed"); e == nil {
		t.Fatalf("delivery_failed not journaled: %+v", readSecretaryJournal(t))
	}
}

// TestMailDeliverByTicketLoadsStoredBody: deliver with only to+ticket pulls the
// original from/subject/body from the stored letter (secretary doesn't reproduce
// the body). Verified via the journal fields, which come from the loaded letter.
func TestMailDeliverByTicketLoadsStoredBody(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Store a letter in the secretary's inbox (as a normal send would).
	stored := mail.Letter{Ticket: "T42", From: "backend", To: "ghost", Subject: "real subject", Body: "real body bytes"}
	if _, err := mail.Deliver(mail.SecretaryAgent, stored); err != nil {
		t.Fatal(err)
	}
	enc, dec := mailTestServer(t, map[string]*SessionInfo{
		"secretary": {Path: filepath.Join(home, "secretary")},
	})
	// Minimal deliver: only to + ticket, NO body/from/subject.
	enc.Encode(APIRequest{Cmd: "mail.deliver", Cwd: filepath.Join(home, "secretary"),
		Payload: json.RawMessage(`{"to":"ghost","ticket":"T42"}`)})
	var r APIResponse
	dec.Decode(&r)
	// ghost has no session so delivery fails — but the event must carry the
	// from/subject LOADED from the stored letter, proving deliver-by-ticket.
	e := journalHas(readSecretaryJournal(t), "T42", "delivery_failed")
	if e == nil {
		t.Fatalf("no delivery_failed for T42: %+v", readSecretaryJournal(t))
	}
	if e.From != "backend" || e.Subject != "real subject" {
		t.Fatalf("deliver did not load stored letter fields: %+v", e)
	}

	// A deliver for an unknown ticket with no body is rejected outright.
	enc.Encode(APIRequest{Cmd: "mail.deliver", Cwd: filepath.Join(home, "secretary"),
		Payload: json.RawMessage(`{"to":"ghost","ticket":"NOPE"}`)})
	var r2 APIResponse
	dec.Decode(&r2)
	if r2.OK || !strings.Contains(r2.Error, "not found") {
		t.Fatalf("expected not-found rejection, got OK=%v err=%q", r2.OK, r2.Error)
	}
}

func TestMailAckLogsEvent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	enc, dec := mailTestServer(t, map[string]*SessionInfo{
		"devops": {Path: filepath.Join(home, "devops")},
	})
	enc.Encode(APIRequest{Cmd: "mail.ack", Cwd: filepath.Join(home, "devops"),
		Payload: json.RawMessage(`{"ticket":"T5"}`)})
	var r APIResponse
	dec.Decode(&r)
	if !r.OK {
		t.Fatalf("ack failed: %s", r.Error)
	}
	e := journalHas(readSecretaryJournal(t), "T5", "acked")
	if e == nil || e.By != "devops" {
		t.Fatalf("acked event missing or wrong actor: %+v", readSecretaryJournal(t))
	}
}

func TestMailSendReplyLogsRepliedEvent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	enc, dec := mailTestServer(t, map[string]*SessionInfo{
		"backend": {Path: filepath.Join(home, "backend")},
	})
	// A reply (in_reply_to set) marks the original ticket replied.
	enc.Encode(APIRequest{Cmd: "mail.send", Cwd: filepath.Join(home, "backend"),
		Payload: json.RawMessage(`{"to":"secretary","subject":"re: x","body":"done","in_reply_to":"T1"}`)})
	var r APIResponse
	dec.Decode(&r)
	if !r.OK {
		t.Fatalf("reply send failed: %s", r.Error)
	}
	e := journalHas(readSecretaryJournal(t), "T1", "replied")
	if e == nil || e.By != "backend" {
		t.Fatalf("replied event missing or wrong actor: %+v", readSecretaryJournal(t))
	}
}

// withMailScheduler installs a temp-backed scheduler for the duration of a test.
func withMailScheduler(t *testing.T) *scheduler.Scheduler {
	t.Helper()
	s, err := scheduler.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mailScheduler = s
	t.Cleanup(func() { mailScheduler = nil })
	return s
}

func TestArmDeliveryTimers(t *testing.T) {
	s := withMailScheduler(t)
	armDeliveryTimers("T1", "backend") // reply expected -> both timers
	if _, ok := s.Get("ack:T1"); !ok {
		t.Fatal("ack timer not armed")
	}
	if _, ok := s.Get("reply:T1"); !ok {
		t.Fatal("reply timer not armed")
	}

	armDeliveryTimers("T2", "") // one-way -> ack only
	if _, ok := s.Get("ack:T2"); !ok {
		t.Fatal("ack timer not armed for T2")
	}
	if _, ok := s.Get("reply:T2"); ok {
		t.Fatal("reply timer should not exist for a one-way letter")
	}
}

func TestAckCancelsAckTimer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := withMailScheduler(t)
	armDeliveryTimers("T7", "backend")

	enc, dec := mailTestServer(t, map[string]*SessionInfo{
		"devops": {Path: filepath.Join(home, "devops")},
	})
	enc.Encode(APIRequest{Cmd: "mail.ack", Cwd: filepath.Join(home, "devops"), Payload: json.RawMessage(`{"ticket":"T7"}`)})
	var r APIResponse
	dec.Decode(&r)
	if !r.OK {
		t.Fatalf("ack failed: %s", r.Error)
	}
	if _, ok := s.Get("ack:T7"); ok {
		t.Fatal("ack timer should be cancelled after ack")
	}
	if _, ok := s.Get("reply:T7"); !ok {
		t.Fatal("reply timer should survive an ack")
	}
}

func TestReplyCancelsReplyTimer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := withMailScheduler(t)
	armDeliveryTimers("T1", "backend")

	enc, dec := mailTestServer(t, map[string]*SessionInfo{
		"devops": {Path: filepath.Join(home, "devops")},
	})
	enc.Encode(APIRequest{Cmd: "mail.send", Cwd: filepath.Join(home, "devops"),
		Payload: json.RawMessage(`{"to":"secretary","subject":"re","body":"done","in_reply_to":"T1"}`)})
	var r APIResponse
	dec.Decode(&r)
	if !r.OK {
		t.Fatalf("reply send failed: %s", r.Error)
	}
	if _, ok := s.Get("reply:T1"); ok {
		t.Fatal("reply timer should be cancelled after the reply arrives")
	}
}

func TestOnMailTimerJournalsTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	onMailTimer(scheduler.Timer{Ticket: "T3", Stage: "ack"})
	e := journalHas(readSecretaryJournal(t), "T3", "timeout")
	if e == nil || e.Stage != "ack" {
		t.Fatalf("timeout event not journaled: %+v", readSecretaryJournal(t))
	}
}
