package mail

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeliverWritesLetterAndJournal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	in := Letter{
		From: "backend", To: "devops", Subject: "deploy v2",
		Body: "please deploy", ReplyTo: "backend", NeedsConfirmation: true,
		Notes: "urgent",
	}
	fileName, err := Deliver(SecretaryAgent, in)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	// Letter lands in the secretary's working dir, not under ~/.ccc/mail.
	wantInbox := filepath.Join(home, ".ccc", "secretary", "inbox")
	if InboxDir(SecretaryAgent) != wantInbox {
		t.Fatalf("InboxDir = %s, want %s", InboxDir(SecretaryAgent), wantInbox)
	}

	data, err := os.ReadFile(filepath.Join(wantInbox, fileName))
	if err != nil {
		t.Fatalf("read letter: %v", err)
	}
	var got Letter
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal letter: %v", err)
	}
	if got.Ticket == "" || got.Timestamp == 0 {
		t.Fatalf("ticket/timestamp not stamped: %+v", got)
	}
	if fileName != got.Ticket+".json" {
		t.Fatalf("fileName %s does not match ticket %s", fileName, got.Ticket)
	}
	if got.From != in.From || got.To != in.To || got.Subject != in.Subject ||
		got.Body != in.Body || got.ReplyTo != in.ReplyTo ||
		!got.NeedsConfirmation || got.Notes != in.Notes {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Journal has exactly one "received" entry referencing the letter file.
	jf, err := os.Open(JournalPath(SecretaryAgent))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer jf.Close()
	var lines int
	var entry JournalEntry
	sc := bufio.NewScanner(jf)
	for sc.Scan() {
		lines++
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			t.Fatalf("unmarshal journal line: %v", err)
		}
	}
	if lines != 1 {
		t.Fatalf("journal lines = %d, want 1", lines)
	}
	if entry.Event != "received" || entry.Ticket != got.Ticket || entry.File != fileName {
		t.Fatalf("journal entry mismatch: %+v", entry)
	}
}

func TestMailboxDirOrdinaryAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := MailboxDir("frontend")
	want := filepath.Join(home, ".ccc", "mail", "frontend")
	if got != want {
		t.Fatalf("MailboxDir(frontend) = %s, want %s", got, want)
	}
}

func TestNewTicketUniqueAndSortable(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 100; i++ {
		tk := NewTicket()
		if seen[tk] {
			t.Fatalf("duplicate ticket: %s", tk)
		}
		seen[tk] = true
		if prev != "" && tk <= prev {
			t.Fatalf("ticket not increasing: %s <= %s", tk, prev)
		}
		prev = tk
	}
}

func TestDeliverRejectsEmptyAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Deliver("", Letter{Subject: "x"}); err == nil || !strings.Contains(err.Error(), "empty agent") {
		t.Fatalf("expected empty-agent error, got %v", err)
	}
}

func TestSecretaryNameAndMailbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Default group keeps the original identity + path.
	if SecretaryName("") != "secretary" || SecretaryName("default") != "secretary" {
		t.Fatal("default group must map to 'secretary'")
	}
	if SecretaryName("research") != "secretary-research" {
		t.Fatalf("named group = %q", SecretaryName("research"))
	}
	// Mailbox paths: default unchanged, named alongside it.
	if MailboxDir("secretary") != home+"/.ccc/secretary" {
		t.Fatalf("default secretary dir = %s", MailboxDir("secretary"))
	}
	if MailboxDir("secretary-research") != home+"/.ccc/secretary-research" {
		t.Fatalf("named secretary dir = %s", MailboxDir("secretary-research"))
	}
	// Ordinary agents still under ~/.ccc/mail.
	if MailboxDir("backend") != home+"/.ccc/mail/backend" {
		t.Fatalf("ordinary dir = %s", MailboxDir("backend"))
	}
	if !IsSecretary("secretary") || !IsSecretary("secretary-research") || IsSecretary("backend") {
		t.Fatal("IsSecretary wrong")
	}
}
