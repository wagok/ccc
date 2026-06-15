package main

// mcp_secretary.go implements `ccc mcp-secretary`: a thin, stateless MCP server
// (JSON-RPC 2.0 over stdio) that each agent registers in its .mcp.json. It owns
// no state — every tool call is translated into a Unix-socket request to the
// running CCC server, which holds the routing/storage logic. This is the
// "submission" interface; delivery and audit happen through Telegram topics.
//
// This file currently wires the DIRECTORY tools (list_agents/get_agent/
// update_self) end-to-end. Mail tools (send/ack/reply/deliver) are added as
// their server-side handlers land (ccc-9vo/ccc-wuv).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kidandcat/ccc/internal/mail"
	"github.com/kidandcat/ccc/internal/registry"
	"github.com/kidandcat/ccc/internal/scheduler"
)

// Delivery-stage deadlines. The tmux stage is synchronous (send-keys success is
// known immediately), so only ack and reply get timers. Defaults are generous;
// the escalation REACTION is the secretary's job, not CCC's.
const (
	ackTimeoutSecs   = 600  // 10 min: recipient should ack quickly after reading
	replyTimeoutSecs = 3600 // 60 min: complex tasks may take a while
)

// mailScheduler is the live delivery-deadline engine. It is nil outside the
// running server (e.g. in unit tests that call handlers directly); all callers
// must nil-check before arming/cancelling.
var mailScheduler *scheduler.Scheduler

// initMailScheduler starts the delivery-deadline engine. Called once from
// listen() after the socket server is up. Overdue timers (deadlines that
// elapsed while CCC was down) fire on start.
func initMailScheduler() error {
	home, _ := os.UserHomeDir()
	s, err := scheduler.New(filepath.Join(home, ".ccc", "scheduler"), onMailTimer)
	if err != nil {
		return err
	}
	mailScheduler = s
	go s.Run(context.Background())
	return nil
}

// onMailTimer fires when a delivery deadline elapses: it journals a timeout
// event and wakes the secretary with the fact. CCC does NOT decide the
// response — escalation lives in the secretary's instruction.
func onMailTimer(t scheduler.Timer) {
	mail.LogEvent(mail.SecretaryAgent, mail.JournalEntry{Ticket: t.Ticket, Event: "timeout", Stage: t.Stage})
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	wakeAgent(cfg, mail.SecretaryAgent, fmt.Sprintf(
		"⏰ Timeout: ticket %s had no %s within the deadline. Escalate per your instructions.", t.Ticket, t.Stage))
}

// armDeliveryTimers arms the ack (always) and reply (only when a reply is
// expected) deadlines for a delivered letter, keyed by ticket+stage so they can
// be cancelled on ack/reply. No-op when the scheduler is not running.
func armDeliveryTimers(ticket, replyTo string) {
	if mailScheduler == nil {
		return
	}
	now := time.Now().Unix()
	mailScheduler.Schedule(scheduler.Timer{
		ID: "ack:" + ticket, FireAt: now + ackTimeoutSecs,
		Kind: "delivery_timeout", Stage: "ack", Ticket: ticket, Agent: mail.SecretaryAgent,
	})
	if replyTo != "" {
		mailScheduler.Schedule(scheduler.Timer{
			ID: "reply:" + ticket, FireAt: now + replyTimeoutSecs,
			Kind: "delivery_timeout", Stage: "reply", Ticket: ticket, Agent: mail.SecretaryAgent,
		})
	}
}

func cancelTimer(id string) {
	if mailScheduler != nil {
		mailScheduler.Cancel(id)
	}
}

// mcpProtocolVersion is the MCP version advertised if the client does not
// request one. We otherwise echo the client's requested version.
const mcpProtocolVersion = "2024-11-05"

// ---------------------------------------------------------------------------
// Merged agent directory view (mechanical config + self-declared card)
// ---------------------------------------------------------------------------

// AgentInfo is the merged directory entry returned by list_agents/get_agent:
// mechanical fields (CCC-owned) plus the agent's self-declared card.
type AgentInfo struct {
	Name         string   `json:"name"`
	Host         string   `json:"host,omitempty"`
	WorkingDir   string   `json:"working_dir,omitempty"`
	TopicID      int64    `json:"topic_id,omitempty"`
	Status       string   `json:"status,omitempty"` // live only in get_agent; omitted in list
	Description  string   `json:"description,omitempty"`
	Areas        []string `json:"areas,omitempty"`
	ContactAbout string   `json:"contact_about,omitempty"`
	CardUpdated  int64    `json:"card_updated,omitempty"`
}

// buildDirectory merges all non-deleted sessions with their self-declared cards.
// Status is intentionally left empty here (cheap, no tmux/SSH); get_agent does
// the live check. Agents that published a card but have no session row are
// included too.
func buildDirectory(cfg *Config) []AgentInfo {
	cards, _ := registry.ListCards()
	cardByAgent := make(map[string]registry.Card, len(cards))
	for _, c := range cards {
		cardByAgent[c.Agent] = c
	}

	var out []AgentInfo
	seen := map[string]bool{}
	for name, info := range cfg.Sessions {
		if info == nil || info.Deleted {
			continue
		}
		ai := AgentInfo{Name: name, Host: info.Host, WorkingDir: info.Path, TopicID: info.TopicID}
		if c, ok := cardByAgent[name]; ok {
			ai.Description, ai.Areas, ai.ContactAbout, ai.CardUpdated = c.Description, c.Areas, c.ContactAbout, c.UpdatedAt
		}
		out = append(out, ai)
		seen[name] = true
	}
	// Card-only agents (published before a session exists).
	for _, c := range cards {
		if seen[c.Agent] {
			continue
		}
		out = append(out, AgentInfo{
			Name: c.Agent, Description: c.Description, Areas: c.Areas,
			ContactAbout: c.ContactAbout, CardUpdated: c.UpdatedAt,
		})
	}
	sortAgentInfo(out)
	return out
}

func sortAgentInfo(a []AgentInfo) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1].Name > a[j].Name; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// getAgentInfo returns one agent's merged info. When live is true and the agent
// has a session, Status is filled via a live tmux check.
func getAgentInfo(cfg *Config, name string, live bool) (AgentInfo, bool) {
	info, ok := cfg.Sessions[name]
	cards, _ := registry.ListCards()
	var card registry.Card
	var hasCard bool
	for _, c := range cards {
		if c.Agent == name {
			card, hasCard = c, true
			break
		}
	}
	if (!ok || info == nil || info.Deleted) && !hasCard {
		return AgentInfo{}, false
	}

	ai := AgentInfo{Name: name}
	if ok && info != nil && !info.Deleted {
		ai.Host, ai.WorkingDir, ai.TopicID = info.Host, info.Path, info.TopicID
		if live {
			ai.Status = checkClaudeState(tmuxSessionName(name), getHostAddress(cfg, info.Host))
		}
	}
	if hasCard {
		ai.Description, ai.Areas, ai.ContactAbout, ai.CardUpdated = card.Description, card.Areas, card.ContactAbout, card.UpdatedAt
	}
	return ai, true
}

// resolveCaller maps a (host, working directory) to the agent (session) that
// owns it. Identity is derived server-side from the caller's real cwd + machine,
// so an agent cannot claim to be another. host is "" for server-local agents and
// the client's HostName for remote ones — needed because two machines may share
// the same path.
func resolveCaller(cfg *Config, host, cwd string) string {
	if cwd == "" {
		return ""
	}
	want := filepath.Clean(cwd)
	for name, info := range cfg.Sessions {
		if info != nil && !info.Deleted && info.Host == host && filepath.Clean(info.Path) == want {
			return name
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Server-side socket handlers (dispatched from handleSocketConnection)
// ---------------------------------------------------------------------------

func handleAgentListCmd(encoder *json.Encoder, cfg *Config) {
	data, err := json.Marshal(buildDirectory(cfg))
	if err != nil {
		encoder.Encode(APIResponse{OK: false, Error: err.Error()})
		return
	}
	encoder.Encode(APIResponse{OK: true, Result: data})
}

func handleAgentGetCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.Name == "" {
		encoder.Encode(APIResponse{OK: false, Error: "get_agent: missing 'name'"})
		return
	}
	info, ok := getAgentInfo(cfg, p.Name, true)
	if !ok {
		encoder.Encode(APIResponse{OK: false, Error: "unknown agent: " + p.Name})
		return
	}
	data, _ := json.Marshal(info)
	encoder.Encode(APIResponse{OK: true, Result: data})
}

func handleAgentUpdateSelfCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	agent := resolveCaller(cfg, req.Host, req.Cwd)
	if agent == "" {
		encoder.Encode(APIResponse{OK: false, Error: "update_self: caller cwd does not match any agent (cwd=" + req.Cwd + ")"})
		return
	}
	var p struct {
		Description  string   `json:"description"`
		Areas        []string `json:"areas"`
		ContactAbout string   `json:"contact_about"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		encoder.Encode(APIResponse{OK: false, Error: "update_self: bad payload: " + err.Error()})
		return
	}
	stored, err := registry.SetCard(registry.Card{
		Agent:        agent,
		Description:  p.Description,
		Areas:        p.Areas,
		ContactAbout: p.ContactAbout,
	})
	if err != nil {
		encoder.Encode(APIResponse{OK: false, Error: err.Error()})
		return
	}
	data, _ := json.Marshal(stored)
	encoder.Encode(APIResponse{OK: true, Result: data})
}

// ---------------------------------------------------------------------------
// Mail: inbound path (agent -> secretary inbox -> wake secretary)
// ---------------------------------------------------------------------------

// wakeAgent posts text to an agent's Telegram topic and injects it into that
// agent's Claude session (starting the session if needed), recording it in
// history. This is the delivery primitive for inter-agent mail: a short inbox
// notification for the secretary, or (later) a full letter for an ordinary
// recipient.
func wakeAgent(cfg *Config, agentName, text string) error {
	info, exists := cfg.Sessions[agentName]
	if !exists || info == nil || info.Deleted {
		return fmt.Errorf("agent %q has no session", agentName)
	}
	if errMsg := ensureSessionRunning(cfg, agentName, info); errMsg != "" {
		return fmt.Errorf("%s", errMsg)
	}
	_, projectName := parseSessionTarget(agentName)
	tmuxName := tmuxSessionName(extractProjectName(projectName))

	if info.TopicID > 0 {
		sendMessage(cfg, cfg.GroupID, info.TopicID, text)
	}
	appendHistory(info.TopicID, HistoryMessage{
		ID:        nextMessageID(),
		Timestamp: time.Now().Unix(),
		From:      "api",
		Text:      text,
		Agent:     "mail",
	})
	if info.TopicID > 0 {
		markTelegramSent(info.TopicID)
	}
	if info.Host != "" {
		return sshTmuxSendKeys(getHostAddress(cfg, info.Host), tmuxName, text)
	}
	return sendToTmux(tmuxName, text)
}

// handleMailSendCmd handles "mail.send": any agent's send lands in the
// secretary's inbox (the recipient is an envelope field, not a delivery
// address). The server stamps the trusted sender and a ticket, persists the
// letter, then wakes the secretary with a short notification.
func handleMailSendCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	from := resolveCaller(cfg, req.Host, req.Cwd)
	if from == "" {
		encoder.Encode(APIResponse{OK: false, Error: "send: caller cwd does not match any agent (cwd=" + req.Cwd + ")"})
		return
	}
	var p struct {
		To                string `json:"to"`
		Subject           string `json:"subject"`
		Body              string `json:"body"`
		ReplyTo           string `json:"reply_to"`
		InReplyTo         string `json:"in_reply_to"`
		NeedsConfirmation bool   `json:"needs_confirmation"`
		Notes             string `json:"notes"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		encoder.Encode(APIResponse{OK: false, Error: "send: bad payload: " + err.Error()})
		return
	}
	if p.To == "" || p.Subject == "" {
		encoder.Encode(APIResponse{OK: false, Error: "send: 'to' and 'subject' are required"})
		return
	}

	letter := mail.Letter{
		Ticket: mail.NewTicket(), From: from, To: p.To, Subject: p.Subject, Body: p.Body,
		ReplyTo: p.ReplyTo, InReplyTo: p.InReplyTo, NeedsConfirmation: p.NeedsConfirmation, Notes: p.Notes,
	}
	if _, err := mail.Deliver(mail.SecretaryAgent, letter); err != nil {
		encoder.Encode(APIResponse{OK: false, Error: "send: persist to secretary inbox: " + err.Error()})
		return
	}

	// The letter is durably stored; waking the secretary is best-effort. If the
	// secretary session is not up yet, it will pick the letter up from its inbox
	// on next start — so we still return success with the ticket.
	notify := fmt.Sprintf("📨 New mail in your inbox — ticket %s, from %s, to %s, subject %q. Process it per your instructions.",
		letter.Ticket, letter.From, letter.To, letter.Subject)
	// A reply (in_reply_to set) also marks the original ticket as replied so the
	// secretary can fold state and cancel that ticket's reply deadline.
	if letter.InReplyTo != "" {
		mail.LogEvent(mail.SecretaryAgent, mail.JournalEntry{
			Ticket: letter.InReplyTo, Event: "replied", By: from, Detail: "via " + letter.Ticket,
		})
		cancelTimer("reply:" + letter.InReplyTo) // reply arrived in time
	}

	result := map[string]string{"ticket": letter.Ticket}
	if err := wakeAgent(cfg, mail.SecretaryAgent, notify); err != nil {
		result["warning"] = "letter stored but secretary not awakened: " + err.Error()
	}
	data, _ := json.Marshal(result)
	encoder.Encode(APIResponse{OK: true, Result: data})
}

// ---------------------------------------------------------------------------
// Mail: outbound path (secretary -> recipient) and acknowledgement
// ---------------------------------------------------------------------------

// renderRecipientLetter formats the full letter injected into an ordinary
// recipient's prompt (ordinary agents have no inbox file — they get the whole
// letter), including how to acknowledge and reply.
func renderRecipientLetter(from, subject, body, replyTo, ticket string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📨 Letter [ticket %s]\nFrom: %s\nSubject: %s\n", ticket, from, subject)
	if replyTo != "" {
		fmt.Fprintf(&b, "Reply requested → %s\n", replyTo)
	}
	fmt.Fprintf(&b, "---\n%s\n---\n", body)
	fmt.Fprintf(&b, "Acknowledge receipt now: ack(ticket=%q).", ticket)
	if replyTo != "" {
		fmt.Fprintf(&b, " When done, reply: send(to=%q, in_reply_to=%q, subject=..., body=...).", replyTo, ticket)
	}
	return b.String()
}

// handleMailDeliverCmd handles "mail.deliver": the secretary forwards a
// validated letter to its real recipient (privileged, secretary-only). The full
// letter is injected into the recipient's prompt and a delivered/delivery_failed
// event is journaled.
func handleMailDeliverCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	if resolveCaller(cfg, req.Host, req.Cwd) != mail.SecretaryAgent {
		encoder.Encode(APIResponse{OK: false, Error: "deliver: secretary-only"})
		return
	}
	var p struct {
		To      string `json:"to"`
		Ticket  string `json:"ticket"`
		From    string `json:"from"`
		Subject string `json:"subject"`
		Body    string `json:"body"`
		ReplyTo string `json:"reply_to"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil {
		encoder.Encode(APIResponse{OK: false, Error: "deliver: bad payload: " + err.Error()})
		return
	}
	if p.To == "" || p.Ticket == "" {
		encoder.Encode(APIResponse{OK: false, Error: "deliver: 'to' and 'ticket' are required"})
		return
	}

	text := renderRecipientLetter(p.From, p.Subject, p.Body, p.ReplyTo, p.Ticket)
	err := wakeAgent(cfg, p.To, text)

	event, detail := "delivered", ""
	if err != nil {
		event, detail = "delivery_failed", err.Error()
	}
	mail.LogEvent(mail.SecretaryAgent, mail.JournalEntry{
		Ticket: p.Ticket, Event: event, To: p.To, From: p.From, Subject: p.Subject, ReplyTo: p.ReplyTo, Detail: detail,
	})
	if err != nil {
		encoder.Encode(APIResponse{OK: false, Error: "deliver: " + err.Error()})
		return
	}
	// Delivery confirmed at the tmux level — arm the ack (and reply) deadlines.
	armDeliveryTimers(p.Ticket, p.ReplyTo)
	encoder.Encode(APIResponse{OK: true})
}

// handleMailAckCmd handles "mail.ack": a recipient confirms it saw a letter.
// The acting agent is the trusted caller. Journals an "acked" event.
func handleMailAckCmd(encoder *json.Encoder, cfg *Config, req APIRequest) {
	by := resolveCaller(cfg, req.Host, req.Cwd)
	if by == "" {
		encoder.Encode(APIResponse{OK: false, Error: "ack: caller cwd does not match any agent (cwd=" + req.Cwd + ")"})
		return
	}
	var p struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(req.Payload, &p); err != nil || p.Ticket == "" {
		encoder.Encode(APIResponse{OK: false, Error: "ack: 'ticket' is required"})
		return
	}
	mail.LogEvent(mail.SecretaryAgent, mail.JournalEntry{Ticket: p.Ticket, Event: "acked", By: by})
	cancelTimer("ack:" + p.Ticket) // acknowledged in time
	encoder.Encode(APIResponse{OK: true})
}

// ---------------------------------------------------------------------------
// Unix-socket client (shim side)
// ---------------------------------------------------------------------------

func socketRoundtrip(req APIRequest) (APIResponse, error) {
	conn, err := net.DialTimeout("unix", socketPath(), 5*time.Second)
	if err != nil {
		return APIResponse{}, fmt.Errorf("cannot reach ccc server: %w", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil { // Encode appends '\n'
		return APIResponse{}, err
	}
	var resp APIResponse
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return APIResponse{}, err
	}
	return resp, nil
}

// Shim transport config, loaded once at startup. On a client machine the local
// CCC has no socket, so requests are relayed to the server over SSH.
var (
	shimClientMode bool
	shimServer     string // SSH target of the server (client mode)
	shimHostName   string // this machine's id, stamped as the trusted caller host
)

func loadShimConfig() {
	cfg, err := loadConfig()
	if err != nil {
		return
	}
	if cfg.Mode == "client" && cfg.Server != "" && cfg.HostName != "" {
		shimClientMode = true
		shimServer = cfg.Server
		shimHostName = cfg.HostName
	}
}

// relayRequest stamps the trusted caller host and routes the request: directly
// to the local socket on the server, or over SSH to `ccc mcp-relay` on the
// server from a client machine.
func relayRequest(req APIRequest) (APIResponse, error) {
	req.Host = shimHostName // "" on the server
	if shimClientMode {
		return sshRelay(req)
	}
	return socketRoundtrip(req)
}

// sshRelay forwards a request to the server's `ccc mcp-relay` over SSH and reads
// back the APIResponse (base64 over the shell, mirroring forwardToServer).
func sshRelay(req APIRequest) (APIResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return APIResponse{}, err
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	cmd := fmt.Sprintf("echo %s | base64 -d | ccc mcp-relay", encoded)
	out, err := runSSH(shimServer, cmd, 20*time.Second)
	if err != nil {
		return APIResponse{}, fmt.Errorf("relay to server: %w", err)
	}
	var resp APIResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &resp); err != nil {
		return APIResponse{}, fmt.Errorf("relay decode: %w (server said: %q)", err, out)
	}
	return resp, nil
}

// mcpRelay is the server side of the client relay: read one APIRequest from
// stdin, run it against the local socket, write the APIResponse to stdout.
func mcpRelay() error {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	os.Stdout.Write(relayProcess(in))
	return nil
}

// relayProcess turns a request payload into a response payload, always emitting
// a well-formed APIResponse (errors become OK:false) so the client shim can
// surface them as tool errors.
func relayProcess(in []byte) []byte {
	var req APIRequest
	if err := json.Unmarshal(bytes.TrimSpace(in), &req); err != nil {
		out, _ := json.Marshal(APIResponse{OK: false, Error: "mcp-relay: bad request: " + err.Error()})
		return append(out, '\n')
	}
	resp, err := socketRoundtrip(req)
	if err != nil {
		resp = APIResponse{OK: false, Error: err.Error()}
	}
	out, _ := json.Marshal(resp)
	return append(out, '\n')
}

// ---------------------------------------------------------------------------
// MCP JSON-RPC 2.0 over stdio
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func isNotification(id json.RawMessage) bool {
	return len(id) == 0 || string(id) == "null"
}

// mcpSecretary runs the stdio JSON-RPC loop until stdin closes.
func mcpSecretary() error {
	loadShimConfig() // decide local-socket vs SSH-relay transport once
	cwd, _ := os.Getwd()
	in := bufio.NewReader(os.Stdin)
	out := json.NewEncoder(os.Stdout)

	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue // unparseable, no id to reply to
		}
		resp, reply := handleRPC(req, cwd)
		if !reply {
			continue // notification
		}
		out.Encode(resp)
	}
}

// handleRPC processes one JSON-RPC message. reply is false for notifications.
func handleRPC(req rpcRequest, cwd string) (rpcResponse, bool) {
	base := rpcResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		ver := p.ProtocolVersion
		if ver == "" {
			ver = mcpProtocolVersion
		}
		base.Result = map[string]interface{}{
			"protocolVersion": ver,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "ccc-secretary", "version": version},
		}
		return base, true

	case "notifications/initialized", "notifications/cancelled":
		return rpcResponse{}, false

	case "ping":
		base.Result = map[string]interface{}{}
		return base, true

	case "tools/list":
		base.Result = map[string]interface{}{"tools": secretaryTools()}
		return base, true

	case "tools/call":
		base.Result = handleToolCall(req.Params, cwd)
		return base, true

	default:
		if isNotification(req.ID) {
			return rpcResponse{}, false
		}
		base.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
		return base, true
	}
}

// secretaryTools returns the MCP tool definitions exposed to agents.
func secretaryTools() []map[string]interface{} {
	obj := func(props map[string]interface{}, required ...string) map[string]interface{} {
		s := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := map[string]interface{}{"type": "string"}
	return []map[string]interface{}{
		{
			"name": "send",
			"description": "Send a letter to another agent THROUGH the secretary. The letter is always submitted to the secretary first, who validates and routes it. Fields: to (intended recipient), subject (theme), body, reply_to (which agent should receive the reply — an agent name, or omit for a one-way informational message), in_reply_to (ticket you are answering), needs_confirmation (when the reply is routed to a third agent, get a short note that it was sent), notes (extra instructions for the secretary). Returns a ticket id.",
			"inputSchema": obj(map[string]interface{}{
				"to":                 str,
				"subject":            str,
				"body":               str,
				"reply_to":           str,
				"in_reply_to":        str,
				"needs_confirmation": map[string]interface{}{"type": "boolean"},
				"notes":              str,
			}, "to", "subject"),
		},
		{
			"name":        "ack",
			"description": "Acknowledge that you have received and read a letter. Call this FIRST thing when a letter arrives, before doing the work. Confirms delivery to the secretary so it stops waiting.",
			"inputSchema": obj(map[string]interface{}{"ticket": str}, "ticket"),
		},
		{
			"name":        "deliver",
			"description": "SECRETARY ONLY. Forward a validated letter to its real recipient: the full letter is injected into the recipient's prompt. Fields: to, ticket, from, subject, body, reply_to.",
			"inputSchema": obj(map[string]interface{}{
				"to": str, "ticket": str, "from": str, "subject": str, "body": str, "reply_to": str,
			}, "to", "ticket"),
		},
		{
			"name":        "list_agents",
			"description": "List all agents known to CCC with their mechanical info (host, working dir, topic) and self-declared card (description, areas, contact_about). Use this to discover who to address a letter to.",
			"inputSchema": obj(map[string]interface{}{}),
		},
		{
			"name":        "get_agent",
			"description": "Get full info for one agent by name, including live status.",
			"inputSchema": obj(map[string]interface{}{"name": str}, "name"),
		},
		{
			"name":        "update_self",
			"description": "Publish/replace YOUR OWN agent card: what you do, your areas of responsibility, and when to contact you. Full replace — omitted fields are cleared. Identity is derived from your working directory; you can only edit your own card.",
			"inputSchema": obj(map[string]interface{}{
				"description":   str,
				"areas":         map[string]interface{}{"type": "array", "items": str},
				"contact_about": str,
			}),
		},
	}
}

// handleToolCall dispatches a tools/call to the matching socket command and
// returns an MCP tool result (content + optional isError).
func handleToolCall(params json.RawMessage, cwd string) map[string]interface{} {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return toolErr("bad tools/call params: " + err.Error())
	}

	switch call.Name {
	case "send":
		resp, err := relayRequest(APIRequest{Cmd: "mail.send", Cwd: cwd, Payload: call.Arguments})
		return resultText(resp, err)
	case "ack":
		resp, err := relayRequest(APIRequest{Cmd: "mail.ack", Cwd: cwd, Payload: call.Arguments})
		return resultText(resp, err)
	case "deliver":
		resp, err := relayRequest(APIRequest{Cmd: "mail.deliver", Cwd: cwd, Payload: call.Arguments})
		return resultText(resp, err)
	case "list_agents":
		resp, err := relayRequest(APIRequest{Cmd: "agent.list"})
		return resultText(resp, err)
	case "get_agent":
		resp, err := relayRequest(APIRequest{Cmd: "agent.get", Payload: call.Arguments})
		return resultText(resp, err)
	case "update_self":
		resp, err := relayRequest(APIRequest{Cmd: "agent.update_self", Cwd: cwd, Payload: call.Arguments})
		return resultText(resp, err)
	default:
		return toolErr("unknown tool: " + call.Name)
	}
}

// resultText turns a socket response into an MCP tool result.
func resultText(resp APIResponse, err error) map[string]interface{} {
	if err != nil {
		return toolErr(err.Error())
	}
	if !resp.OK {
		return toolErr(resp.Error)
	}
	text := string(resp.Result)
	if text == "" {
		text = "ok"
	}
	return toolText(text)
}

func toolText(s string) map[string]interface{} {
	return map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": s}},
	}
}

func toolErr(msg string) map[string]interface{} {
	return map[string]interface{}{
		"isError": true,
		"content": []map[string]interface{}{{"type": "text", "text": msg}},
	}
}
