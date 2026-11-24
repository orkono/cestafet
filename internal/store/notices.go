package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Notice origins.
const (
	OriginUser  = "user"  // the user's own instruction, relayed by a session
	OriginAgent = "agent" // a session's own initiative
)

// MaxBodyBytes caps a notice body.
const MaxBodyBytes = 4096

// BackfillWindow is how far back a newly registered session receives earlier notices.
const BackfillWindow = 24 * time.Hour

// Notice is a message from one session to the other sessions of its project.
type Notice struct {
	ID          int64     `json:"id"`
	Project     string    `json:"project"`
	FromSession string    `json:"from_session"`
	FromProfile string    `json:"from_profile"`
	FromRole    string    `json:"from_role"`
	Origin      string    `json:"origin"`
	PromptText  string    `json:"prompt_text,omitempty"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
	AckedAt     time.Time `json:"acked_at,omitzero"`
	Kind        string    `json:"kind"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	LiftedAt    time.Time `json:"lifted_at,omitzero"`
	Lifts       int64     `json:"lifts,omitempty"`  // the rule this notice lifted
	Guards      []string  `json:"guards,omitempty"` // command patterns that ask before running
}

// NewNotice describes a notice to send.
type NewNotice struct {
	From      Session
	Origin    string
	PromptID  int64 // 0 when not backed by a user prompt
	Body      string
	To        []string      // explicit recipients; empty = every other live session of the project
	Kind      string        // KindInfo (default) or KindRule
	ExpiresIn time.Duration // rules only; 0 = until lifted
	Guards    []string      // rules only; see package guard
	lifts     int64
}

// Send stores a notice and creates a pending delivery for each recipient.
// It returns the notice id and the recipient session ids.
func (s *Store) Send(n NewNotice) (int64, []string, error) {
	body := strings.TrimSpace(n.Body)
	if body == "" {
		return 0, nil, errors.New("notice body is empty")
	}
	if len(body) > MaxBodyBytes {
		return 0, nil, fmt.Errorf("notice body is %d bytes; the limit is %d", len(body), MaxBodyBytes)
	}
	if n.Origin != OriginUser && n.Origin != OriginAgent {
		return 0, nil, fmt.Errorf("invalid origin %q", n.Origin)
	}
	kind, expiresAt, err := s.ruleFields(n)
	if err != nil {
		return 0, nil, err
	}
	guards, err := guardField(kind, n.Guards)
	if err != nil {
		return 0, nil, err
	}
	recipients, err := s.recipients(n)
	if err != nil {
		return 0, nil, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var promptID any
	if n.PromptID > 0 {
		promptID = n.PromptID
	}
	var lifts any
	if n.lifts > 0 {
		lifts = n.lifts
	}
	res, err := tx.Exec(`INSERT INTO notices (project, from_session, from_profile, from_role, origin, prompt_id, body,
		created_at, kind, expires_at, lifts, guards) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.From.Project, n.From.ID, n.From.Profile, n.From.Role, n.Origin, promptID, body, s.unix(),
		kind, expiresAt, lifts, guards)
	if err != nil {
		return 0, nil, fmt.Errorf("insert notice: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, nil, fmt.Errorf("notice id: %w", err)
	}
	for _, to := range recipients {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO deliveries (notice_id, to_session) VALUES (?, ?)`, id, to); err != nil {
			return 0, nil, fmt.Errorf("insert delivery: %w", err)
		}
	}
	return id, recipients, tx.Commit()
}

func (s *Store) recipients(n NewNotice) ([]string, error) {
	if len(n.To) > 0 {
		var out []string
		for _, id := range n.To {
			sess, err := s.Get(id)
			if err != nil {
				return nil, fmt.Errorf("recipient %s: %w", id, err)
			}
			if sess.Project != n.From.Project {
				return nil, fmt.Errorf("recipient %s is in project %q, not %q", id, sess.Project, n.From.Project)
			}
			if id != n.From.ID {
				out = append(out, id)
			}
		}
		return out, nil
	}
	all, err := s.List(n.From.Project)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sess := range all {
		if sess.ID != n.From.ID {
			out = append(out, sess.ID)
		}
	}
	return out, nil
}

// ownSessions selects the session and every earlier session of the same claude
// process (a /clear starts a new id in the same process); its parameters are the
// session id three times. A session does not get its own notices back.
const ownSessions = `SELECT ? UNION SELECT o.id FROM sessions o JOIN sessions me ON me.id = ?
	WHERE me.claude_pid > 0 AND o.claude_pid = me.claude_pid
	AND (o.claude_started = me.claude_started OR o.claude_started = 0 OR me.claude_started = 0) AND o.id != ?`

// Backfill gives a newly registered session the project's recent notices that
// it would otherwise have missed, and every active rule regardless of age.
// Lift notices are skipped: a session that never saw a rule needs no news of
// its end.
func (s *Store) Backfill(sessionID, project string) error {
	now := s.unix()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO deliveries (notice_id, to_session)
		SELECT id, ? FROM notices n WHERE project = ? AND from_session NOT IN (`+ownSessions+`)
		AND ((kind = 'info' AND lifts IS NULL AND created_at >= ?) OR (kind = 'rule' AND `+activeRule+`))`,
		sessionID, project, sessionID, sessionID, sessionID, now-int64(BackfillWindow.Seconds()), now)
	if err != nil {
		return fmt.Errorf("backfill notices: %w", err)
	}
	return nil
}

// noticeBase selects a notice (alias n) with its prompt text (alias p).
const noticeBase = `n.id, n.project, n.from_session, n.from_profile, n.from_role, n.origin,
	COALESCE(p.text, ''), n.body, n.created_at, n.kind, COALESCE(n.expires_at, 0),
	COALESCE(n.lifted_at, 0), COALESCE(n.lifts, 0), n.guards`

// noticeCols adds the delivery's ack time (alias d).
const noticeCols = noticeBase + `, COALESCE(d.acked_at, 0)`

func scanNotices(rows *sql.Rows) ([]Notice, error) {
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		var n Notice
		var created, expires, lifted, acked int64
		var guards string
		if err := rows.Scan(&n.ID, &n.Project, &n.FromSession, &n.FromProfile, &n.FromRole, &n.Origin,
			&n.PromptText, &n.Body, &created, &n.Kind, &expires, &lifted, &n.Lifts, &guards, &acked); err != nil {
			return nil, fmt.Errorf("scan notice: %w", err)
		}
		n.CreatedAt = time.Unix(created, 0)
		n.ExpiresAt = unixOrZero(expires)
		n.LiftedAt = unixOrZero(lifted)
		n.AckedAt = unixOrZero(acked)
		if guards != "" {
			n.Guards = strings.Split(guards, "\n")
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func unixOrZero(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// noticeOverhead estimates the envelope a rendered notice adds around its body.
const noticeOverhead = 320

// TakePending returns all notices not yet delivered to the session and marks
// them delivered.
func (s *Store) TakePending(sessionID string) ([]Notice, error) {
	notices, _, err := s.TakePendingWithin(sessionID, 0)
	return notices, err
}

// TakePendingWithin returns the oldest undelivered notices whose rendered size
// fits in maxBytes (0 means no limit), marks only those delivered and reports
// how many are left for a later hook. At least one notice is always returned
// when any is pending, so a single large notice can never stall delivery.
func (s *Store) TakePendingWithin(sessionID string, maxBytes int) ([]Notice, int, error) {
	// Fast path: avoid taking a write lock when nothing is pending (the common PostToolUse case).
	var has int
	if err := s.db.QueryRow(`SELECT 1 FROM deliveries WHERE to_session = ? AND delivered_at IS NULL LIMIT 1`,
		sessionID).Scan(&has); errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	} else if err != nil {
		return nil, 0, fmt.Errorf("check pending notices: %w", err)
	}

	var take []Notice
	var left int
	err := withImmediateTx(s.db, func(conn *sql.Conn) error {
		ctx := context.Background()
		rows, err := conn.QueryContext(ctx, `SELECT `+noticeCols+` FROM deliveries d
			JOIN notices n ON n.id = d.notice_id
			LEFT JOIN prompts p ON p.id = n.prompt_id
			WHERE d.to_session = ? AND d.delivered_at IS NULL ORDER BY n.id`, sessionID)
		if err != nil {
			return fmt.Errorf("pending notices: %w", err)
		}
		notices, err := scanNotices(rows)
		if err != nil || len(notices) == 0 {
			return err
		}
		take = fitting(notices, maxBytes)
		left = len(notices) - len(take)
		now := s.unix()
		for _, n := range take {
			if _, err := conn.ExecContext(ctx,
				`UPDATE deliveries SET delivered_at = ? WHERE notice_id = ? AND to_session = ? AND delivered_at IS NULL`,
				now, n.ID, sessionID); err != nil {
				return fmt.Errorf("mark delivered: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return take, left, nil
}

// fitting returns the longest prefix of notices within maxBytes, never empty.
func fitting(notices []Notice, maxBytes int) []Notice {
	if maxBytes <= 0 {
		return notices
	}
	size := 0
	for i, n := range notices {
		size += len(n.Body) + len(n.PromptText) + noticeOverhead
		if size > maxBytes && i > 0 {
			return notices[:i]
		}
	}
	return notices
}

// Received lists notices delivered or pending for the session, newest first.
func (s *Store) Received(sessionID string, unackedOnly bool, limit int) ([]Notice, error) {
	q := `SELECT ` + noticeCols + ` FROM deliveries d
		JOIN notices n ON n.id = d.notice_id
		LEFT JOIN prompts p ON p.id = n.prompt_id
		WHERE d.to_session = ?`
	if unackedOnly {
		q += ` AND d.acked_at IS NULL`
	}
	q += ` ORDER BY n.id DESC LIMIT ?`
	rows, err := s.db.Query(q, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("received notices: %w", err)
	}
	return scanNotices(rows)
}

// Ack acknowledges a notice, with an optional short reply.
func (s *Store) Ack(noticeID int64, sessionID, reply string) error {
	res, err := s.db.Exec(`UPDATE deliveries SET acked_at = ?, reply = ?,
		delivered_at = COALESCE(delivered_at, ?) WHERE notice_id = ? AND to_session = ?`,
		s.unix(), reply, s.unix(), noticeID, sessionID)
	if err != nil {
		return fmt.Errorf("ack notice: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("notice %d was not sent to this session: %w", noticeID, ErrNotFound)
	}
	return nil
}

// Delivery is the state of one notice for one recipient.
type Delivery struct {
	Session     string    `json:"session"`
	Profile     string    `json:"profile"`
	Summary     string    `json:"summary,omitempty"`
	DeliveredAt time.Time `json:"delivered_at,omitzero"`
	AckedAt     time.Time `json:"acked_at,omitzero"`
	Reply       string    `json:"reply,omitempty"`
}

// Status returns the delivery state of a notice per recipient.
func (s *Store) Status(noticeID int64) ([]Delivery, error) {
	rows, err := s.db.Query(`SELECT d.to_session, COALESCE(se.profile, ''), COALESCE(se.summary, ''),
			COALESCE(d.delivered_at, 0), COALESCE(d.acked_at, 0), d.reply
		FROM deliveries d LEFT JOIN sessions se ON se.id = d.to_session
		WHERE d.notice_id = ? ORDER BY d.to_session`, noticeID)
	if err != nil {
		return nil, fmt.Errorf("notice status: %w", err)
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var del, ack int64
		if err := rows.Scan(&d.Session, &d.Profile, &d.Summary, &del, &ack, &d.Reply); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		if del > 0 {
			d.DeliveredAt = time.Unix(del, 0)
		}
		if ack > 0 {
			d.AckedAt = time.Unix(ack, 0)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ProjectNotices lists a project's most recent notices, newest first.
func (s *Store) ProjectNotices(project string, limit int) ([]Notice, error) {
	rows, err := s.db.Query(`SELECT `+noticeBase+`, 0
		FROM notices n LEFT JOIN prompts p ON p.id = n.prompt_id
		WHERE n.project = ? ORDER BY n.id DESC LIMIT ?`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("project notices: %w", err)
	}
	return scanNotices(rows)
}
