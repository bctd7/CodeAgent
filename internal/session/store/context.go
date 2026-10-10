package store

import (
	"CodeAgent/internal/llm"
	"CodeAgent/internal/session/config"
	sessql "CodeAgent/internal/session/sql"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func (tx *Tx) SetToolProjection(ctx context.Context, sessionID, entryID string, content *[]llm.ContentBlock) error {
	state := config.ProcessingOriginal
	var encoded any
	if content != nil {
		data, err := json.Marshal(*content)
		if err != nil {
			return err
		}
		encoded = string(data)
		state = config.ProcessingReplaced
	}
	result, err := tx.tx.ExecContext(ctx, sessql.SetToolProjectionSQL, state, encoded, entryID, sessionID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return config.ErrSessionConflict
	}
	return nil
}

func (tx *Tx) MatchToolArguments(ctx context.Context, assistantID, callID string, arguments map[string]any) error {
	var encoded string
	if err := tx.tx.QueryRowContext(ctx, sessql.SelectToolArgumentsSQL, assistantID, callID).Scan(&encoded); err != nil {
		return err
	}
	data, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	if encoded != string(data) {
		return fmt.Errorf("工具结果参数不同于启动参数: %w", config.ErrSessionConflict)
	}
	return nil
}
func (tx *Tx) FindContextSource(ctx context.Context, runID, kind, sourceID string) (string, error) {
	var id string
	err := tx.tx.QueryRowContext(ctx, sessql.SelectContextSourceSQL, runID, kind, sourceID).Scan(&id)
	return id, err
}

func (tx *Tx) InsertCompaction(ctx context.Context, value config.Compaction) error {
	sources, err := json.Marshal(value.SourceIDs)
	if err != nil {
		return err
	}
	origin, err := json.Marshal(value.Origin)
	if err != nil {
		return err
	}
	usage, err := json.Marshal(value.Usage)
	if err != nil {
		return err
	}
	_, err = tx.tx.ExecContext(ctx, sessql.InsertCompactionSQL, value.ID, value.SessionID, value.HeadID, nullText(value.PreviousID), string(sources), value.FirstKeptIndex, value.Summary, string(origin), string(usage), value.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}
func scanCompaction(row interface{ Scan(...any) error }) (config.Compaction, error) {
	var v config.Compaction
	var previous sql.NullString
	var sources, origin, usage, created string
	err := row.Scan(&v.ID, &v.SessionID, &v.HeadID, &previous, &sources, &v.FirstKeptIndex, &v.Summary, &origin, &usage, &created)
	if err != nil {
		return v, err
	}
	v.PreviousID = previous.String
	if err = json.Unmarshal([]byte(sources), &v.SourceIDs); err != nil {
		return v, err
	}
	if err = json.Unmarshal([]byte(origin), &v.Origin); err != nil {
		return v, err
	}
	if err = json.Unmarshal([]byte(usage), &v.Usage); err != nil {
		return v, err
	}
	v.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return v, err
}
func (tx *Tx) GetCompaction(ctx context.Context, id, sessionID string) (config.Compaction, error) {
	return scanCompaction(tx.tx.QueryRowContext(ctx, sessql.SelectCompactionSQL, id, sessionID))
}
func (tx *Tx) ListCompactions(ctx context.Context, sessionID string) ([]config.Compaction, error) {
	rows, err := tx.tx.QueryContext(ctx, sessql.ListCompactionsSQL, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []config.Compaction{}
	for rows.Next() {
		v, err := scanCompaction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (tx *Tx) InsertChildTask(ctx context.Context, v config.ChildTask) error {
	scope, err := json.Marshal(v.FileScope)
	if err != nil {
		return err
	}
	_, err = tx.tx.ExecContext(ctx, sessql.InsertChildTaskSQL, v.ID, v.ParentSessionID, v.ParentRunID, v.ChildSessionID, v.Role, string(scope), v.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}
func scanChildTask(row interface{ Scan(...any) error }) (config.ChildTask, error) {
	var v config.ChildTask
	var scope, created string
	err := row.Scan(&v.ID, &v.ParentSessionID, &v.ParentRunID, &v.ChildSessionID, &v.Role, &scope, &created)
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal([]byte(scope), &v.FileScope); err != nil {
		return v, err
	}
	v.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return v, err
}
func (tx *Tx) GetChildTask(ctx context.Context, id, parentID string) (config.ChildTask, error) {
	return scanChildTask(tx.tx.QueryRowContext(ctx, sessql.SelectChildTaskSQL, id, parentID))
}
func (tx *Tx) ListChildTasks(ctx context.Context, parentID string) ([]config.ChildTask, error) {
	rows, err := tx.tx.QueryContext(ctx, sessql.ListChildTasksSQL, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []config.ChildTask{}
	for rows.Next() {
		v, err := scanChildTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (tx *Tx) IsChildSession(ctx context.Context, sessionID string) (bool, error) {
	var child bool
	err := tx.tx.QueryRowContext(ctx, sessql.IsChildSessionSQL, sessionID).Scan(&child)
	return child, err
}

func (tx *Tx) GetMemoryProgress(ctx context.Context, sessionID, scope string) (config.MemoryProgress, error) {
	v := config.MemoryProgress{SessionID: sessionID, ScopeKey: scope}
	var created string
	err := tx.tx.QueryRowContext(ctx, sessql.SelectMemoryProgressSQL, sessionID, scope).Scan(&v.CoveredEntryID, &created)
	if err == sql.ErrNoRows {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	v.UpdatedAt, err = time.Parse(time.RFC3339Nano, created)
	return v, err
}
func (tx *Tx) PutMemoryProgress(ctx context.Context, v config.MemoryProgress) error {
	_, err := tx.tx.ExecContext(ctx, sessql.UpsertMemoryProgressSQL, v.SessionID, v.ScopeKey, v.CoveredEntryID, v.UpdatedAt.UTC().Format(time.RFC3339Nano))
	return err
}
