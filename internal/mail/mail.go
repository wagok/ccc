// Package mail implements the storage core for inter-agent mail (the "smart
// secretary" system). CCC is dumb transport: it persists an incoming letter to
// the recipient's inbox and appends a journal entry. All routing, validation
// and archival logic lives in the secretary agent itself, which reads these
// files with its normal file tools. CCC only ever APPENDS; it never edits or
// removes letters/journal lines — the owning agent manages its own archive.
package mail

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// SecretaryAgent is the reserved agent identity for the smart secretary. Its
// mailbox lives inside its hardcoded working directory (~/.ccc/secretary),
// unlike ordinary agents whose mailboxes live under ~/.ccc/mail/<agent>.
const SecretaryAgent = "secretary"

// Letter is a single inter-agent message. The recipient is an envelope field
// (To), not a delivery address: every send physically lands in the secretary's
// inbox first, and the secretary delivers it to the real recipient.
type Letter struct {
	Ticket            string `json:"ticket"`                   // unique id, also the inbox filename stem
	From              string `json:"from"`                     // sender agent
	To                string `json:"to"`                       // intended recipient agent
	Subject           string `json:"subject"`                  // theme; validated against recipient's area
	Body              string `json:"body"`                     // message body
	ReplyTo           string `json:"reply_to,omitempty"`       // who the reply is addressed to; "" = one-way
	InReplyTo         string `json:"in_reply_to,omitempty"`    // ticket this message answers
	NeedsConfirmation bool   `json:"needs_confirmation,omitempty"` // notify initiator when reply_to is a third agent
	Notes             string `json:"notes,omitempty"`          // extra instructions for the secretary agent
	Timestamp         int64  `json:"ts"`                       // unix seconds
}

// JournalEntry is an append-only record of a mailbox event. The secretary reads
// the journal to decide what to act on and rewrites/archives it itself.
type JournalEntry struct {
	Ticket    string `json:"ticket"`
	Event     string `json:"event"` // "received"
	From      string `json:"from"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	ReplyTo   string `json:"reply_to,omitempty"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	File      string `json:"file"` // inbox filename for the letter
	Timestamp int64  `json:"ts"`
}

var ticketSeq int64 // process-local counter to disambiguate same-second tickets

// NewTicket returns a unique, human-readable, lexically sortable ticket id such
// as "20260614-153012-0007". Readability matters because tickets are shown to
// the user in Telegram topics.
func NewTicket() string {
	seq := atomic.AddInt64(&ticketSeq, 1)
	return fmt.Sprintf("%s-%04d", time.Now().Format("20060102-150405"), seq%10000)
}

// cccDir returns ~/.ccc.
func cccDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccc")
}

// MailboxDir returns the base mailbox directory for an agent. The secretary's
// mailbox lives inside its fixed working directory; everyone else lives under
// ~/.ccc/mail/<agent>.
func MailboxDir(agent string) string {
	if agent == SecretaryAgent {
		return filepath.Join(cccDir(), "secretary")
	}
	return filepath.Join(cccDir(), "mail", agent)
}

// InboxDir returns the directory holding unread/active letters for an agent.
func InboxDir(agent string) string {
	return filepath.Join(MailboxDir(agent), "inbox")
}

// ArchiveDir returns the directory the agent moves handled letters into. CCC
// never writes here; it exists so the path is well-defined for the agent.
func ArchiveDir(agent string) string {
	return filepath.Join(MailboxDir(agent), "archive")
}

// JournalPath returns the append-only journal file for an agent.
func JournalPath(agent string) string {
	return filepath.Join(MailboxDir(agent), "journal.jsonl")
}

// Deliver persists a letter into an agent's inbox and appends a "received"
// journal entry. It returns the inbox filename written. The Ticket and
// Timestamp are filled in if empty. Waking the agent is the caller's
// responsibility (see the wake mechanism in main.go).
func Deliver(agent string, letter Letter) (string, error) {
	if agent == "" {
		return "", fmt.Errorf("mail.Deliver: empty agent")
	}
	if letter.Ticket == "" {
		letter.Ticket = NewTicket()
	}
	if letter.Timestamp == 0 {
		letter.Timestamp = time.Now().Unix()
	}

	inbox := InboxDir(agent)
	if err := os.MkdirAll(inbox, 0755); err != nil {
		return "", fmt.Errorf("mail.Deliver: mkdir inbox: %w", err)
	}

	fileName := letter.Ticket + ".json"
	filePath := filepath.Join(inbox, fileName)

	data, err := json.MarshalIndent(letter, "", "  ")
	if err != nil {
		return "", fmt.Errorf("mail.Deliver: marshal letter: %w", err)
	}
	// O_EXCL guards against a ticket collision silently overwriting a letter.
	f, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return "", fmt.Errorf("mail.Deliver: write letter %s: %w", fileName, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", fmt.Errorf("mail.Deliver: write letter %s: %w", fileName, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("mail.Deliver: close letter %s: %w", fileName, err)
	}

	if err := appendJournal(agent, JournalEntry{
		Ticket:    letter.Ticket,
		Event:     "received",
		From:      letter.From,
		To:        letter.To,
		Subject:   letter.Subject,
		ReplyTo:   letter.ReplyTo,
		InReplyTo: letter.InReplyTo,
		File:      fileName,
		Timestamp: letter.Timestamp,
	}); err != nil {
		return fileName, fmt.Errorf("mail.Deliver: append journal: %w", err)
	}

	return fileName, nil
}

// appendJournal appends one newline-delimited JSON entry to the agent's
// journal. O_APPEND keeps small writes atomic, mirroring appendHistory.
func appendJournal(agent string, entry JournalEntry) error {
	if err := os.MkdirAll(MailboxDir(agent), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(JournalPath(agent), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(entry)
}
