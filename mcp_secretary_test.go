package main

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/kidandcat/ccc/internal/registry"
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
	for _, want := range []string{"list_agents", "get_agent", "update_self"} {
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
		"backend": {Path: "/home/x/proj/backend"},
		"dead":    {Path: "/home/x/proj/dead", Deleted: true},
	}}
	if got := resolveCaller(cfg, "/home/x/proj/backend"); got != "backend" {
		t.Fatalf("resolveCaller = %q, want backend", got)
	}
	if got := resolveCaller(cfg, "/home/x/proj/backend/"); got != "backend" {
		t.Fatalf("resolveCaller (trailing slash) = %q, want backend", got)
	}
	if got := resolveCaller(cfg, "/home/x/proj/other"); got != "" {
		t.Fatalf("resolveCaller unknown = %q, want empty", got)
	}
	if got := resolveCaller(cfg, "/home/x/proj/dead"); got != "" {
		t.Fatalf("resolveCaller deleted = %q, want empty", got)
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
