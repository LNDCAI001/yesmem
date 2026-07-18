package storage

import (
	"fmt"
	"time"

	"github.com/LNDCAI001/yesmem/internal/models"
)

// UpdateLearningCategoryAndContent flips a learning's category and rewrites
// its content in a single statement. Used by the cap-proposal-decide flow to
// transition a proposal from category='cap_proposed' into either
// 'cap_proposed_accepted' or 'cap_proposed_rejected', optionally appending a
// reviewer note to the content (since models.Learning has no separate notes
// column).
func (s *Store) UpdateLearningCategoryAndContent(id int64, category, content string) error {
	res, err := s.db.Exec(
		`UPDATE learnings SET category = ?, content = ? WHERE id = ?`,
		category, content, id,
	)
	if err != nil {
		return fmt.Errorf("update learning category: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("learning %d not found", id)
	}
	return nil
}

// ListLearningsByCategory returns rows with the given category, optionally
// scoped to a project. Unlike GetActiveLearnings it does NOT filter by
// superseded_by — proposal rows are never superseded; their lifecycle is
// expressed via category transitions instead. Keywords and anticipated_queries
// live in their own tables and are intentionally not hydrated here — callers
// (list_cap_proposals) only need the row body.
func (s *Store) ListLearningsByCategory(category, project string, limit int) ([]*models.Learning, error) {
	if limit <= 0 {
		limit = 100
	}
	args := []any{category}
	q := `SELECT id, content, category, source, project, confidence, context, domain,
		trigger_rule, created_at, content_hash
		FROM learnings WHERE category = ?`
	if project != "" {
		q += " AND project = ?"
		args = append(args, project)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list learnings by category: %w", err)
	}
	defer rows.Close()

	var out []*models.Learning
	for rows.Next() {
		var l models.Learning
		var createdAt string
		if err := rows.Scan(
			&l.ID, &l.Content, &l.Category, &l.Source, &l.Project, &l.Confidence,
			&l.Context, &l.Domain, &l.TriggerRule,
			&createdAt, &l.ContentHash,
		); err != nil {
			return nil, fmt.Errorf("scan learning: %w", err)
		}
		l.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, &l)
	}
	return out, rows.Err()
}
