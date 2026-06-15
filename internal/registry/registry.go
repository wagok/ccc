// Package registry stores the self-declared agent cards for the smart-secretary
// system — the "what I do / what I'm responsible for / when to contact me"
// information that each agent publishes about ITSELF via MCP update_self.
//
// This is only the self-declared layer. The mechanical layer (status, working
// dir, host, topic) lives in CCC's config + live state and is merged with these
// cards in main.go when answering list_agents/get_agent. Keeping card CRUD here
// (one JSON file per agent) makes it unit-testable in isolation and avoids
// cross-agent write contention: each agent owns exactly its own file.
package registry

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Card is an agent's self-declared description. update_self does a FULL replace
// of these fields (the agent re-publishes its whole card), so omitting a field
// clears it.
type Card struct {
	Agent        string   `json:"agent"`                   // whose card this is
	Description  string   `json:"description,omitempty"`   // free text: what the agent does
	Areas        []string `json:"areas,omitempty"`         // areas of responsibility / topics
	ContactAbout string   `json:"contact_about,omitempty"` // when to write to this agent
	UpdatedAt    int64    `json:"updated_at"`              // unix seconds, stamped on write
}

// Dir returns the registry directory (~/.ccc/registry).
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ccc", "registry")
}

// cardFile maps an agent name to its card file. The name is URL-escaped so that
// names containing path separators (e.g. "team/backend") map to a single safe
// filename; the true agent name is always read back from the file content.
func cardFile(agent string) string {
	return filepath.Join(Dir(), url.QueryEscape(agent)+".json")
}

// SetCard fully replaces an agent's card and stamps UpdatedAt. The agent name
// is taken from c.Agent (the trusted caller identity in main.go), not from
// untrusted input. Returns the stored card.
func SetCard(c Card) (Card, error) {
	c.Agent = strings.TrimSpace(c.Agent)
	if c.Agent == "" {
		return Card{}, fmt.Errorf("registry.SetCard: empty agent")
	}
	c.UpdatedAt = time.Now().Unix()

	if err := os.MkdirAll(Dir(), 0755); err != nil {
		return Card{}, err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return Card{}, err
	}
	// Write-then-rename so a concurrent reader never sees a half-written card.
	path := cardFile(c.Agent)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return Card{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return Card{}, err
	}
	return c, nil
}

// GetCard returns an agent's card. ok is false if the agent has not published
// one yet.
func GetCard(agent string) (card Card, ok bool, err error) {
	data, err := os.ReadFile(cardFile(agent))
	if err != nil {
		if os.IsNotExist(err) {
			return Card{}, false, nil
		}
		return Card{}, false, err
	}
	if err := json.Unmarshal(data, &card); err != nil {
		return Card{}, false, err
	}
	return card, true, nil
}

// ListCards returns every published card, ordered by agent name. The agent name
// comes from each file's content, not its filename.
func ListCards() ([]Card, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cards []Card
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(Dir(), e.Name()))
		if err != nil {
			continue
		}
		var c Card
		if json.Unmarshal(data, &c) == nil && c.Agent != "" {
			cards = append(cards, c)
		}
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Agent < cards[j].Agent })
	return cards, nil
}

// DeleteCard removes an agent's card. Unknown agents are a no-op.
func DeleteCard(agent string) error {
	if err := os.Remove(cardFile(agent)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
