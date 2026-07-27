package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ResponseStateRow is the persisted record for one Responses API output.
// It's the source of truth for `previous_response_id` replay: the handler
// reads it before forwarding and appends the prior items to the new
// canonical Chat request, then writes a fresh row when the new response
// completes.
type ResponseStateRow struct {
	ID            string
	CreatedAt     time.Time
	TTLAt         time.Time
	Fingerprint   string
	ItemsEnvelope json.RawMessage
	UsageEnvelope json.RawMessage
}

// ConversationRow is the ordered list of response IDs that belong to a
// single conversation. The handler uses it to enforce the relationship
// between `conversation` and `previous_response_id` and to detect forks.
type ConversationRow struct {
	ID             string
	CreatedAt      time.Time
	LastResponseID string
	ResponseIDs    []string
}

// PutResponseState writes (or replaces) a response_state row. The conversation
// append happens via AppendConversationResponse; this function is just the
// per-response write.
func (s *DB) PutResponseState(ctx context.Context, r ResponseStateRow) error {
	if r.ItemsEnvelope == nil {
		r.ItemsEnvelope = json.RawMessage(`{}`)
	}
	if r.UsageEnvelope == nil {
		r.UsageEnvelope = json.RawMessage(`{}`)
	}
	_, err := s.ExecContext(ctx, `
        INSERT INTO response_state (id, created_at, ttl_at, request_fingerprint, items_json, usage_json)
        VALUES (?,?,?,?,?,?)
        ON CONFLICT(id) DO UPDATE SET
            ttl_at=excluded.ttl_at,
            request_fingerprint=excluded.request_fingerprint,
            items_json=excluded.items_json,
            usage_json=excluded.usage_json`,
		r.ID,
		r.CreatedAt.Unix(),
		r.TTLAt.Unix(),
		r.Fingerprint,
		string(r.ItemsEnvelope),
		string(r.UsageEnvelope),
	)
	return err
}

// GetResponseState fetches a non-expired row or returns sql.ErrNoRows.
func (s *DB) GetResponseState(ctx context.Context, id string) (ResponseStateRow, error) {
	var r ResponseStateRow
	var createdAt, ttlAt int64
	var itemsJSON, usageJSON string
	err := s.QueryRowContext(ctx, `
        SELECT id, created_at, ttl_at, request_fingerprint, items_json, usage_json
        FROM response_state WHERE id = ? AND ttl_at > ?`, id, time.Now().Unix()).
		Scan(&r.ID, &createdAt, &ttlAt, &r.Fingerprint, &itemsJSON, &usageJSON)
	if err != nil {
		return r, err
	}
	r.CreatedAt = time.Unix(createdAt, 0)
	r.TTLAt = time.Unix(ttlAt, 0)
	r.ItemsEnvelope = json.RawMessage(itemsJSON)
	r.UsageEnvelope = json.RawMessage(usageJSON)
	return r, nil
}

// PutConversation inserts a new conversation row. The caller is responsible
// for generating a conversation ID (usually a conv_<ULID>).
func (s *DB) PutConversation(ctx context.Context, c ConversationRow) error {
	if c.ResponseIDs == nil {
		c.ResponseIDs = []string{}
	}
	ids, err := json.Marshal(c.ResponseIDs)
	if err != nil {
		return err
	}
	_, err = s.ExecContext(ctx, `
        INSERT INTO conversation (id, created_at, last_response_id, response_ids_json)
        VALUES (?,?,?,?)`,
		c.ID, c.CreatedAt.Unix(), c.LastResponseID, string(ids))
	return err
}

// GetConversation returns the row for a conversation ID.
func (s *DB) GetConversation(ctx context.Context, id string) (ConversationRow, error) {
	var c ConversationRow
	var createdAt int64
	var lastID sql.NullString
	var idsJSON string
	err := s.QueryRowContext(ctx, `
        SELECT id, created_at, last_response_id, response_ids_json
        FROM conversation WHERE id = ?`, id).
		Scan(&c.ID, &createdAt, &lastID, &idsJSON)
	if err != nil {
		return c, err
	}
	c.CreatedAt = time.Unix(createdAt, 0)
	if lastID.Valid {
		c.LastResponseID = lastID.String
	}
	if err := json.Unmarshal([]byte(idsJSON), &c.ResponseIDs); err != nil {
		c.ResponseIDs = nil
	}
	return c, nil
}

// AppendConversationResponse atomically appends a response ID to a
// conversation and updates last_response_id. Both updates succeed or both
// roll back. Returns ErrConversationNotFound if no row exists — the handler
// is expected to have created the row before persistence begins.
func (s *DB) AppendConversationResponse(ctx context.Context, conversationID, responseID string) error {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var idsJSON string
	err = tx.QueryRowContext(ctx, `
        SELECT response_ids_json FROM conversation WHERE id = ?`,
		conversationID).Scan(&idsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConversationNotFound
	}
	if err != nil {
		return err
	}
	var ids []string
	if err := json.Unmarshal([]byte(idsJSON), &ids); err != nil {
		ids = []string{}
	}
	// Reject duplicate appends to keep the chain linear.
	for _, existing := range ids {
		if existing == responseID {
			return nil // idempotent
		}
	}
	ids = append(ids, responseID)
	out, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
        UPDATE conversation SET last_response_id = ?, response_ids_json = ?
        WHERE id = ?`,
		responseID, string(out), conversationID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ErrConversationNotFound is returned when a transaction expects an existing
// conversation row but the row is missing. Callers should propagate this as
// a 4xx to the client (e.g. invalid_request_error) rather than auto-creating
// a row out of thin air.
var ErrConversationNotFound = errors.New("store: conversation not found")

// ResponseStateGC deletes expired response_state rows. Returns the number of
// rows removed. Reparent conversation.last_response_id when it points at a
// row that just got deleted; drop empty conversations at the end. The
// caller is responsible for cadence (mirroring cache.StartGC).
func (s *DB) ResponseStateGC(ctx context.Context) (int64, error) {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
        DELETE FROM response_state WHERE ttl_at <= ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()

	// Drop last_response_id pointers into non-existent rows.
	if _, err := tx.ExecContext(ctx, `
        UPDATE conversation SET last_response_id = NULL
        WHERE last_response_id IS NOT NULL
          AND last_response_id NOT IN (SELECT id FROM response_state)`); err != nil {
		return n, err
	}

	// Walk each conversation and prune dead IDs from its list, then drop
	// rows whose list became empty.
	rows, err := tx.QueryContext(ctx, `SELECT id, response_ids_json FROM conversation`)
	if err != nil {
		return n, err
	}
	defer rows.Close()

	type convRow struct {
		id  string
		ids []string
	}
	var convs []convRow
	for rows.Next() {
		var id, idsJSON string
		if err := rows.Scan(&id, &idsJSON); err != nil {
			return n, err
		}
		var ids []string
		if err := json.Unmarshal([]byte(idsJSON), &ids); err != nil {
			ids = []string{}
		}
		convs = append(convs, convRow{id: id, ids: ids})
	}
	if err := rows.Close(); err != nil {
		return n, err
	}

	for _, c := range convs {
		filtered := c.ids[:0]
		for _, id := range c.ids {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM response_state WHERE id = ?`, id).Scan(&exists); err == nil && exists > 0 {
				filtered = append(filtered, id)
			}
		}
		if len(filtered) == 0 {
			_, _ = tx.ExecContext(ctx, `DELETE FROM conversation WHERE id = ?`, c.id)
			continue
		}
		out, _ := json.Marshal(filtered)
		var last string
		if len(filtered) > 0 {
			last = filtered[len(filtered)-1]
		}
		_, _ = tx.ExecContext(ctx, `
            UPDATE conversation SET response_ids_json = ?, last_response_id = ?
            WHERE id = ?`, string(out), last, c.id)
	}

	if err := tx.Commit(); err != nil {
		return n, err
	}
	return n, nil
}
