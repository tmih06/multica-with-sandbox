package factory

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ticketStore persists ticket state and exposes active runs for reconciliation.
// Implementations must serialize updates so a process restart cannot observe a
// partially written ticket.
type ticketStore interface {
	Create(TicketInput) (Ticket, error)
	Get(string) (Ticket, bool, error)
	Update(string, func(*Ticket)) error
	Active() ([]Ticket, error)
	AddEvent(string, string, string) (Event, error)
	EventsAfter(string, int64) ([]Event, error)
	Close() error
}

// Event is one persisted lifecycle or worker-output notification.
type Event struct {
	ID        int64     `json:"id"`
	TicketID  string    `json:"ticket_id"`
	Type      string    `json:"type"`
	Data      string    `json:"data"`
	CreatedAt time.Time `json:"created_at"`
}

// sqliteStore stores factory state in one SQLite database. SQLite is sufficient
// for the single factory process and gives the MVP restart durability without
// requiring another service.
type sqliteStore struct {
	db *sql.DB
}

// OpenSQLiteStore opens or creates a factory database and applies its schema.
func OpenSQLiteStore(path string) (*sqliteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &sqliteStore{db: db}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS tickets (
 id TEXT PRIMARY KEY,
 title TEXT NOT NULL,
 prompt TEXT NOT NULL,
 repository TEXT NOT NULL DEFAULT '',
 model TEXT NOT NULL DEFAULT '',
 policy_model TEXT NOT NULL DEFAULT '',
 policy_thinking TEXT NOT NULL DEFAULT '',
 policy_max_turns INTEGER NOT NULL DEFAULT 0,
 policy_timeout_seconds INTEGER NOT NULL DEFAULT 0,
 policy_cpu_request TEXT NOT NULL DEFAULT '',
 policy_memory_request TEXT NOT NULL DEFAULT '',
 policy_network_profile TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL,
 sandbox TEXT NOT NULL DEFAULT '',
 answer TEXT NOT NULL DEFAULT '',
 logs TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 started_at TEXT NOT NULL DEFAULT '',
 finished_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS factory_sequence (
 name TEXT PRIMARY KEY,
 value INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 ticket_id TEXT NOT NULL,
 type TEXT NOT NULL,
 data TEXT NOT NULL,
 created_at TEXT NOT NULL,
 FOREIGN KEY(ticket_id) REFERENCES tickets(id)
);
INSERT INTO factory_sequence(name, value)
 VALUES ('ticket', 0)
 ON CONFLICT(name) DO NOTHING;
`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := ensurePolicyColumns(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func ensurePolicyColumns(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(tickets)")
	if err != nil {
		return err
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for name, definition := range map[string]string{
		"policy_model": "TEXT NOT NULL DEFAULT ''", "policy_thinking": "TEXT NOT NULL DEFAULT ''",
		"policy_max_turns": "INTEGER NOT NULL DEFAULT 0", "policy_timeout_seconds": "INTEGER NOT NULL DEFAULT 0",
		"policy_cpu_request": "TEXT NOT NULL DEFAULT ''", "policy_memory_request": "TEXT NOT NULL DEFAULT ''",
		"policy_network_profile": "TEXT NOT NULL DEFAULT ''",
	} {
		if !columns[name] {
			if _, err := db.Exec("ALTER TABLE tickets ADD COLUMN " + name + " " + definition); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *sqliteStore) Create(input TicketInput) (Ticket, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback()
	var sequence uint64
	if err := tx.QueryRow("SELECT value FROM factory_sequence WHERE name = 'ticket'").Scan(&sequence); err != nil {
		return Ticket{}, err
	}
	sequence++
	if _, err := tx.Exec("UPDATE factory_sequence SET value = ? WHERE name = 'ticket'", sequence); err != nil {
		return Ticket{}, err
	}
	ticket := newTicket(input, fmt.Sprintf("ticket-%d", sequence))
	if _, err := tx.Exec(`INSERT INTO tickets
(id, title, prompt, repository, model, policy_model, policy_thinking, policy_max_turns, policy_timeout_seconds, policy_cpu_request, policy_memory_request, policy_network_profile, status, sandbox, answer, logs, error, created_at, started_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ticketValues(ticket)...); err != nil {
		return Ticket{}, err
	}
	if err := tx.Commit(); err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

func (s *sqliteStore) Get(id string) (Ticket, bool, error) {
	row := s.db.QueryRow(ticketSelect+" WHERE id = ?", id)
	ticket, err := scanTicket(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, false, nil
	}
	return ticket, err == nil, err
}

func (s *sqliteStore) Update(id string, fn func(*Ticket)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ticket, err := scanTicket(tx.QueryRow(ticketSelect+" WHERE id = ?", id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("ticket %s not found", id)
		}
		return err
	}
	fn(&ticket)
	if _, err := tx.Exec(`UPDATE tickets SET title=?, prompt=?, repository=?, model=?, policy_model=?, policy_thinking=?, policy_max_turns=?, policy_timeout_seconds=?, policy_cpu_request=?, policy_memory_request=?, policy_network_profile=?, status=?, sandbox=?, answer=?, logs=?, error=?, created_at=?, started_at=?, finished_at=? WHERE id=?`, append(ticketUpdateValues(ticket), ticket.ID)...); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sqliteStore) Active() ([]Ticket, error) {
	rows, err := s.db.Query(ticketSelect + " WHERE status IN ('queued', 'running') ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tickets []Ticket
	for rows.Next() {
		ticket, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, ticket)
	}
	return tickets, rows.Err()
}

func (s *sqliteStore) AddEvent(ticketID, eventType, data string) (Event, error) {
	createdAt := time.Now().UTC()
	result, err := s.db.Exec("INSERT INTO events(ticket_id, type, data, created_at) VALUES (?, ?, ?, ?)", ticketID, eventType, data, formatTime(createdAt))
	if err != nil {
		return Event{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Event{}, err
	}
	return Event{ID: id, TicketID: ticketID, Type: eventType, Data: data, CreatedAt: createdAt}, nil
}

func (s *sqliteStore) EventsAfter(ticketID string, after int64) ([]Event, error) {
	rows, err := s.db.Query("SELECT id, ticket_id, type, data, created_at FROM events WHERE ticket_id = ? AND id > ? ORDER BY id", ticketID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var created string
		if err := rows.Scan(&event.ID, &event.TicketID, &event.Type, &event.Data, &created); err != nil {
			return nil, err
		}
		event.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *sqliteStore) Close() error { return s.db.Close() }

const ticketSelect = `SELECT id, title, prompt, repository, model, policy_model, policy_thinking, policy_max_turns, policy_timeout_seconds, policy_cpu_request, policy_memory_request, policy_network_profile, status, sandbox, answer, logs, error, created_at, started_at, finished_at FROM tickets`

type scanner interface{ Scan(...any) error }

func scanTicket(row scanner) (Ticket, error) {
	var ticket Ticket
	var created, started, finished string
	if err := row.Scan(&ticket.ID, &ticket.Title, &ticket.Prompt, &ticket.Repository, &ticket.Model, &ticket.Policy.Model, &ticket.Policy.Thinking, &ticket.Policy.MaxTurns, &ticket.Policy.TimeoutSeconds, &ticket.Policy.CPURequest, &ticket.Policy.MemoryRequest, &ticket.Policy.NetworkProfile, &ticket.Status, &ticket.Sandbox, &ticket.Answer, &ticket.Logs, &ticket.Error, &created, &started, &finished); err != nil {
		return Ticket{}, err
	}
	var err error
	if ticket.CreatedAt, err = parseTime(created); err != nil {
		return Ticket{}, err
	}
	if ticket.StartedAt, err = parseOptionalTime(started); err != nil {
		return Ticket{}, err
	}
	if ticket.FinishedAt, err = parseOptionalTime(finished); err != nil {
		return Ticket{}, err
	}
	ticket.Policy = effectivePolicy(TicketInput{Model: ticket.Model, Policy: ticket.Policy})
	return ticket, nil
}

func ticketValues(ticket Ticket) []any {
	return []any{ticket.ID, ticket.Title, ticket.Prompt, ticket.Repository, ticket.Model, ticket.Policy.Model, ticket.Policy.Thinking, ticket.Policy.MaxTurns, ticket.Policy.TimeoutSeconds, ticket.Policy.CPURequest, ticket.Policy.MemoryRequest, ticket.Policy.NetworkProfile, ticket.Status, ticket.Sandbox, ticket.Answer, ticket.Logs, ticket.Error, formatTime(ticket.CreatedAt), formatOptionalTime(ticket.StartedAt), formatOptionalTime(ticket.FinishedAt)}
}

func ticketUpdateValues(ticket Ticket) []any {
	return []any{ticket.Title, ticket.Prompt, ticket.Repository, ticket.Model, ticket.Policy.Model, ticket.Policy.Thinking, ticket.Policy.MaxTurns, ticket.Policy.TimeoutSeconds, ticket.Policy.CPURequest, ticket.Policy.MemoryRequest, ticket.Policy.NetworkProfile, ticket.Status, ticket.Sandbox, ticket.Answer, ticket.Logs, ticket.Error, formatTime(ticket.CreatedAt), formatOptionalTime(ticket.StartedAt), formatOptionalTime(ticket.FinishedAt)}
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return formatTime(value)
}
func parseTime(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) }
func parseOptionalTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return parseTime(value)
}
